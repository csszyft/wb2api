package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

func (p *Panel) proxyReady(w http.ResponseWriter) bool {
	if p.cfg.Egress == nil {
		writeErr(w, http.StatusNotImplemented, "代理池未装配")
		return false
	}
	return true
}

func decodeProxyBody(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return errors.New("请求读取失败或超过 2 MB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(out); err != nil {
		return errors.New("请求不是有效 JSON 或内容过大")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("请求包含多余内容")
	}
	return nil
}

func (p *Panel) proxyAccounts(realm string, ids []string) ([]egress.Account, error) {
	if realm != "" && realm != "cn" && realm != "global" {
		return nil, errors.New("账号区域无效")
	}
	selected := map[string]bool{}
	for _, uid := range ids {
		selected[uid] = true
	}
	accounts := []egress.Account{}
	for _, a := range p.cfg.Pool.List() {
		if (realm != "" && a.Realm != realm) || (len(ids) > 0 && !selected[a.UID]) {
			continue
		}
		accounts = append(accounts, egress.Account{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm})
		delete(selected, a.UID)
	}
	if len(selected) > 0 {
		return nil, errors.New("所选账号不存在或不在指定区域")
	}
	return accounts, nil
}

func (p *Panel) refreshProxyAccounts() error {
	if p.cfg.Pool.NotifyAccountChange() {
		return nil
	}
	accounts, _ := p.proxyAccounts("", nil)
	return p.cfg.Egress.SyncAccounts(accounts)
}

func (p *Panel) proxiesList(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	if err := p.refreshProxyAccounts(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, p.cfg.Egress.Snapshot())
}

func (p *Panel) proxiesSettings(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var settings egress.Settings
	if err := decodeProxyBody(r, &settings); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.refreshProxyAccounts(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := p.cfg.Egress.UpdateSettings(settings); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type proxyImportRequest struct {
	Protocol string `json:"protocol"`
	Text     string `json:"text"`
	Base     string `json:"base"`
	Count    int    `json:"count"`
	Prefix   string `json:"prefix"`
	Capacity int    `json:"capacity"`
	Assign   bool   `json:"assign"`
	Replace  bool   `json:"replace"`
	Realm    string `json:"realm"`
}

func (p *Panel) proxiesImport(w http.ResponseWriter, r *http.Request) {
	p.importProxyLinks(w, r, false)
}
func (p *Panel) proxiesGenerate(w http.ResponseWriter, r *http.Request) {
	p.importProxyLinks(w, r, true)
}

func (p *Panel) importProxyLinks(w http.ResponseWriter, r *http.Request, generate bool) {
	if !p.proxyReady(w) {
		return
	}
	p.proxyMutationMu.Lock()
	defer p.proxyMutationMu.Unlock()
	if job := p.cfg.Egress.JobSnapshot(); job != nil && job.Status == "running" {
		writeErr(w, 409, "代理检测仍在执行，请等待完成")
		return
	}
	var request proxyImportRequest
	if err := decodeProxyBody(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if request.Capacity == 0 {
		request.Capacity = p.cfg.Egress.Snapshot().Settings.DefaultCapacity
	}
	urls, problems := egress.ExtractURLs(request.Text, request.Protocol)
	if generate {
		var err error
		urls, err = egress.StickyURLs(request.Base, request.Count, request.Prefix)
		problems = nil
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if len(urls) == 0 {
		writeErr(w, 400, "未找到可导入的代理；请使用 协议://用户名:密码@主机:端口")
		return
	}
	var accounts []egress.Account
	if request.Assign {
		var err error
		accounts, err = p.proxyAccounts(request.Realm, nil)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if err := p.refreshProxyAccounts(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids, added, duplicates, err := p.cfg.Egress.Import(urls, request.Capacity)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	job, err := p.cfg.Egress.StartCheck(ids, accounts, request.Replace)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "imported": added, "duplicates": duplicates, "errors": problems, "job": job})
}

func (p *Panel) proxiesCheck(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	p.proxyMutationMu.Lock()
	defer p.proxyMutationMu.Unlock()
	var request struct {
		IDs    []string `json:"ids"`
		Assign bool     `json:"assign"`
		Realm  string   `json:"realm"`
	}
	if err := decodeProxyBody(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(request.IDs) == 0 {
		for _, proxy := range p.cfg.Egress.Snapshot().Proxies {
			if proxy.Enabled {
				request.IDs = append(request.IDs, proxy.ID)
			}
		}
	}
	var accounts []egress.Account
	if request.Assign {
		var err error
		accounts, err = p.proxyAccounts(request.Realm, nil)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	job, err := p.cfg.Egress.StartCheck(request.IDs, accounts, false)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job": job})
}

func (p *Panel) proxiesAssign(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var request struct {
		Realm   string   `json:"realm"`
		UIDs    []string `json:"uids"`
		Replace bool     `json:"replace"`
	}
	if err := decodeProxyBody(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	accounts, err := p.proxyAccounts(request.Realm, request.UIDs)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.refreshProxyAccounts(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	result, err := p.cfg.Egress.Assign(accounts, request.Replace)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

func (p *Panel) proxyUpdate(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var request struct {
		Name     string `json:"name"`
		Capacity int    `json:"capacity"`
		Enabled  bool   `json:"enabled"`
	}
	if err := decodeProxyBody(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.cfg.Egress.UpdateProxy(r.PathValue("id"), request.Name, request.Capacity, request.Enabled); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (p *Panel) proxyRemove(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	if err := p.cfg.Egress.RemoveProxy(r.PathValue("id")); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (p *Panel) accountProxy(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	uid := r.PathValue("uid")
	if p.cfg.Pool.AuthByUID(uid) == nil {
		writeErr(w, 404, "账号不存在")
		return
	}
	var request struct {
		ProxyID string `json:"proxy_id"`
	}
	if err := decodeProxyBody(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.cfg.Egress.Bind(uid, request.ProxyID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "route": p.cfg.Egress.Route(uid)})
}
