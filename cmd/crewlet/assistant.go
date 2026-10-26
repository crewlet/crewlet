package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/assistantapi"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tabular"
)

// mountAssistant builds the Dashboard Assistant over this node's engine and
// hands its routes, its grants and their dispatcher to opts. It runs after
// [api.HumanSurfaces.Mount], because the writes a person may propose are the
// operator surface's verdict ([operator.Server.Acts]) and that surface is
// handed over there.
//
// BUILT ON EVERY NODE THAT SERVES THE API, whether or not the company turns
// the assistant on: the service reads the company per turn, so an apply that
// turns it on is the next question's, and `/viewer` says why it is off.
//
// The service is returned, with this node's half of its fleet stream, so the
// caller closes both before its listeners shut down: an open stream is a
// request a listener's grace would otherwise wait out whole.
func mountAssistant(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	host engine.DeepHost, opts *api.Options) (*assistantNode, error) {
	delegations, err := auth.NewDelegations(boot.Secrets.TokenMaterial(), e, nil)
	if err != nil {
		return nil, fmt.Errorf("assistant: grants: %w", err)
	}
	dispatcher := auth.NewDispatcher(delegations)
	acts := noActs
	if opts.Operator != nil {
		acts = opts.Operator.Acts
	}
	// THE FLEET HALF, nil on a node with no peer: a turn running on another
	// node is followed and stopped through it, and every frame this node
	// appends is told to the fleet, encrypted.
	fleet, err := engine.AssistantFleet(e)
	if err != nil {
		return nil, fmt.Errorf("assistant: fleet: %w", err)
	}
	var remote assistant.Remote
	if fleet != nil {
		remote = assistant.FleetRemote(fleet)
	}
	// THE CATALOGUE: the curated tools now, and the generic API tools once
	// the API this node builds next binds its route table ([bindRoutes]).
	tools := &catalog.API{}
	svc, err := assistant.New(assistant.Deps{
		Node:        e.Node().ID(),
		Company:     func() (*config.Company, *org.Organization) { return companyConfig(e) },
		Acts:        acts,
		Models:      engine.AssistantModels(e),
		Budget:      engine.AssistantBudget(e),
		Catalogue:   tools,
		Transcripts: e.AssistantReader(),
		Records:     engine.AssistantRecords(e),
		Dispatcher:  dispatcher,
		Revoker:     delegations,
		Audit:       e.Backends().Queue,
		Metrics:     e.Recorder(),
		Remote:      remote,
		Rate:        engine.AssistantTurnRate(e),
		Files:       engine.AssistantFiles(e),
		Deep:        engine.AssistantDeepWork(e, boot, host),
		Sessions:    engine.AssistantToolSessions(e),
	})
	if err != nil {
		return nil, err
	}
	grants, err := assistantapi.Grants(delegations)
	if err != nil {
		return nil, err
	}
	store, err := approval.Domain(e.AssistantWriter(), e.AssistantReader())
	if err != nil {
		return nil, err
	}
	approver, err := approval.New(approval.Deps{
		Store: store, Grants: grants, Dispatcher: dispatcher, Turns: svc,
		Revisions: catalog.Revision,
		StepUp: func(act string) bool {
			recency, ok := authz.RecencyOf(authz.Action(act))
			return ok && recency == iam.RecencyStepUp
		},
		StepOp:   statelog.StepOpID,
		DomainOf: engine.LogDomain,
	})
	if err != nil {
		return nil, err
	}
	if err = svc.BindDecliner(approver); err != nil {
		return nil, err
	}
	audits, err := assistantapi.DomainAudits(e.AssistantWriter(), e.AssistantReader())
	if err != nil {
		return nil, err
	}
	// THE FILE ROUTES, owner's and audited, only where this node keeps
	// files: a nil store is a nil interface here, never a typed nil.
	var auditFiles assistantapi.AuditFiles
	if store := engine.AssistantFileStore(e); store != nil {
		auditFiles = store
	}
	decisions, err := assistantapi.NewDecisions(assistantapi.DecisionsDeps{
		Approver: approver, Audits: audits, Render: svc.Render, Audit: e.Backends().Queue,
		Files: auditFiles,
	})
	if err != nil {
		return nil, err
	}
	surface, err := assistantapi.New(assistantapi.Options{
		Service: svc, Grants: grants, Tables: tabular.Tables{},
		Chart: engine.ChartAuthorityOf(e), Decisions: decisions,
		Files: engine.AssistantFiles(e),
	})
	if err != nil {
		return nil, err
	}
	// ANSWERING PEERS LAST, once nothing left can fail: a node that serves
	// the fleet's replays and stops has a service behind them to answer.
	node := &assistantNode{svc: svc, tools: tools}
	if fleet != nil {
		if err := fleet.Start(ctx, svc); err != nil {
			closeAssistant(ctx, node, logging.Get("assistant"))
			return nil, fmt.Errorf("assistant: fleet: %w", err)
		}
		node.fleet = fleet
	}
	opts.Delegations, opts.Dispatcher, opts.Assistant = delegations, dispatcher, surface
	return node, nil
}

// assistantNode is the assistant's service and this node's half of its fleet
// stream, closed together, and the catalogue the API's routes are bound to.
type assistantNode struct {
	svc   *assistant.Service
	fleet *live.Fleet
	tools *catalog.API
}

// bindRoutes hands the API's route table to the assistant's catalogue, once
// the API is built: what the generic API tools may name (ADR-0052).
func (n *assistantNode) bindRoutes(app *api.App) error {
	if err := n.tools.Bind(app.AssistantRoutes()); err != nil {
		return fmt.Errorf("assistant: %w", err)
	}
	return nil
}

// noActs is the verdict on a node with no operator surface: no write is
// offered, so every turn there reads.
func noActs(context.Context, iam.Principal) ([]string, error) { return nil, nil }

// closeAssistant stops every turn the service runs, within the API's shutdown
// grace, saying so when one did not record its end in time, and then
// withdraws this node from the fleet stream, so the frames those ends wrote
// are told first. Nil-safe.
func closeAssistant(ctx context.Context, n *assistantNode, log *slog.Logger) {
	if n == nil {
		return
	}
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiShutdownGrace)
	defer cancel()
	if err := n.svc.Close(grace); err != nil {
		log.WarnContext(ctx, "assistant_close_incomplete", "error", err,
			"hint", "a turn this node was running may not have recorded that it was "+
				"interrupted before the shutdown grace ran out")
	}
	if n.fleet == nil {
		return
	}
	if err := n.fleet.Close(grace); err != nil {
		log.WarnContext(ctx, "assistant_fleet_not_withdrawn", "error", err,
			"hint", "this node may still be named as an answerer of the fleet's replays "+
				"and stops until its broker connection closes")
	}
}
