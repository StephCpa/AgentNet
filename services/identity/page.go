package identity

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
)

type agentView struct{ ID, Handle string }
type profileView struct {
	ID string
	Profile
}
type consentView struct {
	ID, Agent, Instance string
	Version             int64
	Expires             time.Time
	Revoked             *time.Time
	Rights              authority.Rights
	Limits              map[string]string
}
type pageView struct {
	Principal, CSRF string
	Agents          []agentView
	Profiles        []profileView
	Consents        []consentView
}

var pageTemplate = template.Must(template.New("control").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AgentNet · 用户授权</title>
<style>body{font:16px/1.6 system-ui,sans-serif;background:#f5f7fa;color:#172337;max-width:960px;margin:40px auto;padding:0 24px}h1{font-size:30px}section{background:white;border:1px solid #d6dfeb;border-radius:10px;padding:22px;margin:22px 0}label{display:block;margin:10px 0}input,select,button{font:inherit;padding:7px;max-width:100%;box-sizing:border-box}button{background:#174ca4;color:white;border:0;border-radius:5px;cursor:pointer}small{color:#556477}dt{font-weight:600}dd{margin:0 0 12px}code{overflow-wrap:anywhere}</style></head>
<body><h1>用户授权</h1>{{if not .Principal}}<section><p>登录后，可为自己的 Agent 批准权限和每任务预算，也可撤销已有授权。</p><a href="/login">使用组织账户登录</a></section>{{else}}
<p>当前用户：<code>{{.Principal}}</code></p>
<form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>退出登录</button></form>
<h2>批准新授权</h2><p>每份授权仅适用于所选 Agent。额度是每个任务的上限；有效期内，该 Agent 可据此创建多个任务。</p>
{{$root:=.}}{{range .Profiles}}<section><h3>{{.Name}}</h3>
<dl><dt>允许的操作</dt><dd>{{range .Rights.Actions}}<code>{{.}}</code> {{end}}</dd><dt>允许的资源</dt><dd>{{range .Rights.Resources}}<code>{{.}}</code> {{end}}</dd><dt>允许的数据类别</dt><dd>{{range .Rights.DataClasses}}{{.}} {{end}}</dd><dt>执行范围</dt><dd>本域、通过网关执行，不允许向其他 Agent 委托。</dd></dl>
<form method="post" action="/consents"><input type="hidden" name="csrf" value="{{$root.CSRF}}"><input type="hidden" name="action" value="create"><input type="hidden" name="profile" value="{{.ID}}">
<label>Agent <select required name="agent">{{range $root.Agents}}<option value="{{.ID}}">{{.Handle}}</option>{{end}}</select></label>
<label>限定实例（留空表示该 Agent 的所有已登记实例）<input name="instance" placeholder="实例编号"></label>
<label>有效小时数（最多 {{.MaxHours}} 小时）<input type="number" name="hours" min="1" max="{{.MaxHours}}" value="1" required></label>
{{range $dim,$amount:=.TaskLimits}}<label>{{$dim}} 每任务额度（最多 {{$amount}}）<input name="{{$dim}}" value="{{$amount}}" inputmode="decimal" required></label>{{end}}
<button>批准以上权限与额度</button></form></section>{{end}}
<h2>已有授权</h2>{{range .Consents}}<section><h3><code>{{.ID}}</code></h3><p>Agent：<code>{{.Agent}}</code>{{if .Instance}} · 限定实例：<code>{{.Instance}}</code>{{end}}</p><p>到期：{{.Expires.UTC.Format "2006-01-02 15:04:05 UTC"}}</p>
<p>操作：{{range .Rights.Actions}}{{.}} {{end}} · 资源：{{range .Rights.Resources}}{{.}} {{end}} · 数据类别：{{range .Rights.DataClasses}}{{.}} {{end}}</p><p>每任务额度：{{range $dim,$amount:=.Limits}}{{$dim}} {{$amount}}　{{end}}</p>
{{if .Revoked}}<strong>已撤销</strong>{{else}}<form method="post" action="/consents"><input type="hidden" name="csrf" value="{{$root.CSRF}}"><input type="hidden" name="action" value="revoke"><input type="hidden" name="consent" value="{{.ID}}"><input type="hidden" name="version" value="{{.Version}}"><button>撤销授权</button></form>{{end}}</section>{{else}}<p>尚无授权。</p>{{end}}
<small>撤销会阻止新操作。已发出的业务请求仍需对账；已发生的费用不会被撤销。</small>{{end}}</body></html>`))

func (s *Service) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	view := pageView{}
	if _, err := r.Cookie(sessionCookie); err == nil {
		tx, err := s.db.Begin(r.Context())
		if err != nil {
			s.fail(w, r, 503, "database")
			return
		}
		defer tx.Rollback(r.Context())
		r = withTransaction(r, tx)
		principal, session, err := s.session(r, tx)
		if err != nil {
			cookie(w, sessionCookie, "", -1)
			s.fail(w, r, 401, "invalid session")
			return
		}
		view.Principal = principal
		view.CSRF = csrf(session)
		rows, err := tx.Query(r.Context(), `SELECT agent_id,handle FROM agents WHERE domain_id=$1 AND owner_principal_id=$2 AND status='active' ORDER BY agent_id`, s.cfg.Domain, principal)
		if err != nil {
			s.fail(w, r, 503, "agents lookup")
			return
		}
		for rows.Next() {
			var a agentView
			if err = rows.Scan(&a.ID, &a.Handle); err != nil {
				break
			}
			view.Agents = append(view.Agents, a)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			s.fail(w, r, 503, "agents read")
			return
		}
		rows, err = tx.Query(r.Context(), `SELECT consent_id,actor_agent_id,COALESCE(actor_instance_id,''),version,expires_at,revoked_at,rights,task_limits FROM root_consents WHERE domain_id=$1 AND subject_principal_id=$2 ORDER BY consent_id`, s.cfg.Domain, principal)
		if err != nil {
			s.fail(w, r, 503, "consents lookup")
			return
		}
		for rows.Next() {
			var c consentView
			var rights, limits []byte
			if err = rows.Scan(&c.ID, &c.Agent, &c.Instance, &c.Version, &c.Expires, &c.Revoked, &rights, &limits); err != nil {
				break
			}
			if err = json.Unmarshal(rights, &c.Rights); err != nil {
				break
			}
			if err = json.Unmarshal(limits, &c.Limits); err != nil {
				break
			}
			view.Consents = append(view.Consents, c)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			s.fail(w, r, 503, "consents read")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			s.fail(w, r, 503, "page snapshot")
			return
		}
		for id, p := range s.cfg.Profiles {
			view.Profiles = append(view.Profiles, profileView{id, p})
		}
		sort.Slice(view.Profiles, func(i, j int) bool { return view.Profiles[i].ID < view.Profiles[j].ID })
	}
	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, view); err != nil {
		s.fail(w, r, 500, "page rendering")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}
