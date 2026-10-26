package rpc_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/agentnet/agentnet/adapters/mockbiz"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/delivery"
	"github.com/agentnet/agentnet/services/execution"
	"github.com/agentnet/agentnet/services/tcs"
)

type networkResolver struct{ client tcsv1.TaskControlClient }

func (r networkResolver) Resolve(ctx context.Context, e tcs.Envelope) (tcs.Effect, error) {
	response, err := r.client.ReportEffectResult(ctx, &tcsv1.ReportEffectResultRequest{SignedResult: pb(e)})
	if err != nil {
		return tcs.Effect{}, err
	}
	return tcs.Effect{ID: response.EffectId, State: response.State}, nil
}

func TestRPCExecutionConsentAtSend(t *testing.T) {
	for _, change := range []string{"live", "revoked", "version", "data-class", "domain", "chain-live", "chain-revoked"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			var a *tcsv1.AdmissionResult
			var ancestor string
			if change == "chain-live" || change == "chain-revoked" {
				c := grantChain(t, f)
				a = c[2]
				ancestor = c[1].RootGrantId
			} else {
				a = f.submit()
			}
			proposal, _ := f.proposal(a)
			if _, err := f.control.AdmitEffect(f.ctx, proposal); err != nil {
				t.Fatal(err)
			}
			queue, err := delivery.New(f.db, delivery.Config{Domain: f.domain, Destination: "local-control"})
			if err != nil {
				t.Fatal(err)
			}
			inbox, err := delivery.NewInbox(f.db, f.domain, "local-control")
			if err != nil {
				t.Fatal(err)
			}
			for {
				worked, err := queue.Once(f.ctx, inbox)
				if err != nil {
					t.Fatal(err)
				}
				if !worked {
					break
				}
			}
			params := execution.Files{Root: t.TempDir()}
			digest, err := params.Put(f.ctx, []byte(`{"amount":"2"}`))
			if err != nil || digest != proposal.Effect.ParamsDigest {
				t.Fatal(digest, err)
			}
			if err = mockbiz.Install(f.ctx, f.db); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(&mockbiz.Server{DB: f.db, Namespace: f.domain, Capability: "queryable"})
			defer server.Close()
			client := mockbiz.Client{URL: server.URL, HTTP: server.Client()}
			adapter := tcsv1.NewTaskControlClient(f.connect(f.adapterCert))
			engine, err := execution.New(f.db, execution.Config{Domain: f.domain, ResultKeyID: "adapter", ResultKey: f.adapterKey, Targets: map[string]execution.Binding{"bank": {Capability: "queryable", Target: client}}, Payloads: params, Resolver: networkResolver{adapter}})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "chain-revoked":
				f.exec("UPDATE grants SET revoked_at=clock_timestamp(),status='revoked' WHERE grant_id=$1", ancestor)
			case "revoked":
				f.exec(`UPDATE root_consents SET revoked_at=clock_timestamp(),version=version+1 WHERE consent_id=$1`, f.consent)
			case "version":
				f.exec(`UPDATE root_consents SET version=version+1 WHERE consent_id=$1`, f.consent)
			case "data-class":
				f.exec(`UPDATE grants SET rights=jsonb_set(rights,'{data_classes}','["public"]') WHERE grant_id=$1`, a.RootGrantId)
			case "domain":
				f.exec(`UPDATE grants SET rights=jsonb_set(rights,'{spatial,domains}','["different-domain"]') WHERE grant_id=$1`, a.RootGrantId)
			}
			if err = engine.Execute(f.ctx, proposal.Effect.EffectId); err != nil {
				t.Fatal(err)
			}
			business := f.count(`SELECT count(*) FROM mockbiz.calls WHERE namespace=$1`, f.domain)
			held := f.count(`SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'`, proposal.Effect.EffectId)
			if change == "live" || change == "chain-live" {
				snapshot, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
				if err != nil || business != 1 || held != 0 || len(snapshot.GetEffects()) != 1 || snapshot.Effects[0].State != "COMMITTED" {
					t.Fatalf("live authorized execution: business=%d held=%d snapshot=%v error=%v", business, held, snapshot, err)
				}
			} else if business != 0 || held != 4 {
				t.Fatalf("authority change permitted send: business=%d held=%d", business, held)
			}
		})
	}
}
