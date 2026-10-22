package egress

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Job struct {
	ID         string        `json:"id"`
	Status     string        `json:"status"`
	Total      int           `json:"total"`
	Done       int           `json:"done"`
	OK         int           `json:"ok"`
	Failed     int           `json:"failed"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	Errors     []string      `json:"errors"`
	Assignment *AssignResult `json:"assignment,omitempty"`
}

func (m *Manager) JobSnapshot() *Job {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	if m.job == nil {
		return nil
	}
	out := *m.job
	out.Errors = append([]string{}, m.job.Errors...)
	if m.job.Assignment != nil {
		a := *m.job.Assignment
		a.Unbound = append([]string{}, a.Unbound...)
		out.Assignment = &a
	}
	return &out
}

// StartCheck returns before network work starts. The job survives a closed tab;
// bounded workers detect proxies, then optionally assign the selected accounts.
func (m *Manager) StartCheck(ids []string, assign []Account, replace bool) (*Job, error) {
	if len(ids) == 0 || len(ids) > MaxProxies {
		return nil, errors.New("请选择需要检测的代理")
	}
	m.mu.RLock()
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if _, ok := m.index[id]; !ok {
			m.mu.RUnlock()
			return nil, errors.New("所选代理不存在，请刷新列表")
		}
		if !seen[id] {
			unique = append(unique, id)
			seen[id] = true
		}
	}
	m.mu.RUnlock()
	ids = unique
	m.jobMu.Lock()
	if m.job != nil && m.job.Status == "running" {
		m.jobMu.Unlock()
		return nil, errors.New("代理检测仍在执行，请等待完成")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.jobCancel = cancel
	done := make(chan struct{})
	m.jobDone = done
	m.job = &Job{ID: time.Now().UTC().Format("20060102T150405.000000000"), Status: "running", Total: len(ids), StartedAt: time.Now(), Errors: []string{}}
	jobID := m.job.ID
	m.jobMu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		ch := make(chan string)
		var workers sync.WaitGroup
		for worker := 0; worker < min(3, len(ids)); worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for id := range ch {
					err := m.Check(ctx, id)
					m.jobMu.Lock()
					m.job.Done++
					if err == nil {
						m.job.OK++
					} else {
						m.job.Failed++
						if len(m.job.Errors) < 50 {
							m.job.Errors = append(m.job.Errors, id+"："+err.Error())
						}
					}
					m.jobMu.Unlock()
				}
			}()
		}
	feed:
		for _, id := range ids {
			select {
			case <-ctx.Done():
				break feed
			case ch <- id:
			}
		}
		close(ch)
		workers.Wait()
		var assignment *AssignResult
		var assignErr error
		if ctx.Err() == nil && assign != nil {
			a, err := m.Assign(assign, replace)
			assignment, assignErr = &a, err
		}
		if ctx.Err() == nil && assign == nil {
			m.mu.Lock()
			if m.state.Settings.Enabled && m.state.Settings.AutoAssign {
				next := cloneState(m.state)
				a := assignLocked(&next, m.accounts, false)
				if a.Assigned > 0 {
					assignment, assignErr = &a, m.saveLocked(next)
				}
			}
			m.mu.Unlock()
		}
		m.jobMu.Lock()
		if m.job.ID == jobID {
			m.job.Assignment = assignment
			m.job.Status = "done"
			if ctx.Err() != nil {
				m.job.Status = "cancelled"
			}
			if assignErr != nil {
				m.job.Status = "failed"
				m.job.Errors = append(m.job.Errors, assignErr.Error())
			}
			m.job.FinishedAt = time.Now()
		}
		m.jobMu.Unlock()
	}()
	return m.JobSnapshot(), nil
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.jobMu.Lock()
	cancel, done := m.jobCancel, m.jobDone
	m.jobMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
