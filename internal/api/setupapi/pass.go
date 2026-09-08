package setupapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"

	"github.com/google/uuid"
)

// Running a third-party app's provisioning from the dashboard.
//
// # Two routes, one machine
//
// `provision` runs the pass with a sink and the public base, which is what
// makes it able to mint a credential and register a webhook. `check` runs the
// SAME pass with neither, which is what makes it read-only: a person asking
// "is it working now" after fixing something at the third-party app gets a fresh
// answer without the engine writing anything.
//
// # The result goes where the loop's does
//
// Both write their outcome to the same fleet status the reconcile loop
// writes, through the same fold, so the Integrations screen updates with no
// extra plumbing and a pass run by hand can never disagree with a tick that
// runs a minute later.

// The refusals these routes add.
const (
	codePassInFlight      = httpjson.Code("pass_in_flight")
	codeNotProvisionable  = httpjson.Code("not_provisionable")
	codeNoPublicBaseURL   = httpjson.Code("no_public_base_url")
	codeRequirementsShort = httpjson.Code("requirements_outstanding")
	codeRunNotFound       = httpjson.Code("run_not_found")
	codeVendorRefused     = httpjson.Code("vendor_refused")
)

// Status is where a pass records what it found.
//
// The consumer's own interface over the fleet's integration status: save one,
// forget one. The loop's store satisfies it, which is the point — one record,
// two writers.
type Status interface {
	SaveIntegration(ctx context.Context, state integration.State) error
	ForgetIntegration(ctx context.Context, kind integration.Kind) error
	LoadIntegrations(ctx context.Context) ([]integration.State, error)
}

// provisionRequest is what the dashboard sends.
type provisionRequest struct {
	Seats []string `json:"seats"`
	// DryRun plans and validates without writing at the third-party app.
	DryRun bool `json:"dry_run"`
	// Recreate re-registers hooks with a fresh secret. DESTRUCTIVE across
	// deployments, so the dashboard gates it behind a typed confirmation
	// and this route names that in its own answer.
	Recreate bool `json:"recreate_webhooks"`
	// OperatorCredential is the transient third-party app administrator credential
	// a pass may need. Never stored, never logged, and zeroed as soon as
	// the pass returns.
	OperatorCredential string `json:"operator_credential"`
}

// provision serves POST /setup/integrations/{kind}/provision.
func (s *Service) provision(w http.ResponseWriter, r *http.Request) {
	s.runPass(w, r, false)
}

// check serves POST /setup/integrations/{kind}/check.
func (s *Service) check(w http.ResponseWriter, r *http.Request) {
	s.runPass(w, r, true)
}

// runPass is both routes: the only difference is whether the pass is handed
// the two things that let it write.
func (s *Service) runPass(w http.ResponseWriter, r *http.Request, readOnly bool) {
	kind := integration.Kind(r.PathValue("kind"))
	if !s.passes.Serves(kind) {
		httpjson.FailWith(w, http.StatusConflict, codeNotProvisionable, map[string]string{
			"detail": "this build runs no provisioning pass for " + string(kind),
			"hint": "its setup is the values on this surface; anything at the " +
				"third-party app is done there or with the crewlet command line",
		})
		return
	}
	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active",
		})
		return
	}
	state, ok := s.state(company, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, codeUnknownKind, map[string]string{
			"hint": "one of " + kindList(),
		})
		return
	}

	var req provisionRequest
	if !readOnly {
		body, err := httpjson.ReadBody(w, r, MaxBody)
		if err != nil {
			httpjson.Refuse(w, err)
			return
		}
		if len(body) > 0 {
			if err := decode(body, &req); err != nil {
				httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
					map[string]string{"detail": err.Error()})
				return
			}
		}
		// A PASS DOES NOT COLLECT INPUTS. Every value it needs is either
		// already in the document or asked for here as a transient
		// credential, so an incomplete configuration is refused with the
		// list rather than run half way.
		if missing := setup.Outstanding(state.Requirements); len(missing) > 0 {
			names := make([]string, 0, len(missing))
			for _, m := range missing {
				names = append(names, m.Field)
			}
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"error": string(codeRequirementsShort), "fields": names,
				"hint": "supply these first; a pass writes at the third-party app and " +
					"must not run against a half-configured integration",
			})
			return
		}
	}

	in := setup.PassInput{
		Seats: req.Seats, DryRun: req.DryRun, Recreate: req.Recreate,
		Operator: req.OperatorCredential,
	}
	if !readOnly {
		base := company.Integrations.WebhookBase(s.resolve)
		if base == "" {
			// SUPPLYING THE BASE IS THE PERMISSION TO REGISTER, so a pass
			// with none would run and register nothing while reporting
			// success. Refused by name instead.
			httpjson.FailWith(w, http.StatusConflict, codeNoPublicBaseURL, map[string]string{
				"config_path": "integrations.public_base_url",
				"hint": "set the HTTPS address third-party apps reach this deployment on; " +
					"without it the pass can register no webhook",
			})
			return
		}
		in.WebhookBase = base
		sink, err := s.sink(operatorOf(r))
		if err != nil {
			httpjson.FailWith(w, http.StatusServiceUnavailable, codeNoKeyring, map[string]string{
				"detail": "this node has no secrets.keys, so a minted credential cannot be sealed",
				"hint":   "run `crewlet secrets keygen` and install one",
			})
			return
		}
		in.Sink = sink
	}

	// DETACHED FROM THE REQUEST, and bounded on its own.
	//
	// A pass WRITES AT THE VENDOR: it creates accounts, mints tokens and
	// registers webhooks. Run on the request's own context, a browser tab
	// closing or a reverse proxy hitting its read timeout cancels it
	// mid-way, and what is left behind is an account created with no
	// credential sealed, or a credential sealed with no pointer written.
	// The next pass then either duplicates the account or reports a
	// half-finished integration nobody asked for. Nothing about the
	// operator's connection should decide that.
	//
	// The deadline is the LEASE's, not a guess: the fleet lease that stops
	// two operators minting at once is not renewed mid-pass, so a pass
	// outliving it would be running unprotected. Timing out just inside it
	// keeps the two facts in step.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), PassDeadline)
	defer cancel()
	run, err := s.passes.Start(ctx, kind, in, uuid.NewString())
	// THE TRANSIENT CREDENTIAL IS DROPPED THE MOMENT THE PASS RETURNS.
	// It can create accounts at the third-party app, and the difference between a
	// one-time grant and a standing power is exactly how long it is held.
	in.Operator = ""
	req.OperatorCredential = ""
	switch {
	case errors.Is(err, setup.ErrPassInFlight):
		httpjson.FailWith(w, http.StatusConflict, codePassInFlight, map[string]string{
			"hint": "another pass for this integration is running; wait for it " +
				"rather than minting twice",
		})
		return
	case errors.Is(err, setup.ErrNoPass):
		httpjson.Fail(w, http.StatusConflict, codeNotProvisionable)
		return
	case run == nil:
		// The pass never started: the lease could not be read, which is
		// three-valued and is NOT evidence that somebody else is minting.
		// There is nothing to record, because nothing was observed.
		log.ErrorContext(r.Context(), "setup_pass_not_started",
			"integration", kind, "error", err.Error(), "operator", operatorOf(r))
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeInternalError,
			map[string]string{
				"hint": "the coordination store could not say whether another node " +
					"is already provisioning this integration; try again",
			})
		return
	}

	// THE STATUS IS WRITTEN WHETHER OR NOT THE PASS SUCCEEDED, through the
	// fold the loop uses. A pass that failed is a fact about the
	// integration, and hiding it here would leave the screen showing the
	// last good answer under a fresh timestamp.
	s.record(r.Context(), kind, run, err)

	if err != nil {
		log.ErrorContext(r.Context(), "setup_pass_failed",
			"integration", kind, "run", run.ID, "error", err.Error(),
			"operator", operatorOf(r))
		httpjson.Write(w, http.StatusBadGateway, map[string]any{
			"error": string(codeVendorRefused), "run": run,
			// The run carries the third-party app's own sentence, which is what an
			// operator needs. It is safe here because the one refusal that
			// could quote a config value no longer does.
			"hint": "the third-party app refused this pass; nothing it had already done " +
				"is undone, and re-running is safe",
		})
		return
	}
	log.InfoContext(r.Context(), "setup_pass_ran",
		"integration", kind, "run", run.ID, "read_only", readOnly,
		"findings", len(run.Findings), "operator", operatorOf(r))
	httpjson.Write(w, http.StatusOK, run)
}

// recordEndpoint remembers the address a surface was set up against, for a
// surface whose address only a person can change ([integration.IngressOperator]).
//
// A ROW WITH NOTHING ELSE IN IT, which is the honest shape: no pass has
// observed this surface, so there is no phase, no finding and no attempt to
// report. What the row carries is the one fact only this write knows, and a
// reader that finds a report on it is reading one some other writer put
// there.
func (s *Service) recordEndpoint(ctx context.Context, kind integration.Kind, base string) {
	if s.status == nil || base == "" {
		return
	}
	release, held, err := s.passes.Hold(ctx, kind)
	if err != nil || !held {
		log.WarnContext(ctx, "setup_endpoint_unrecorded",
			"integration", kind, "error", errorOrBusy(err),
			"detail", "another writer holds this surface, so the address is "+
				"not recorded and a later change of the public base URL will "+
				"not be reported for it")
		return
	}
	defer release()

	current, err := s.currentState(ctx, kind)
	if err != nil {
		log.WarnContext(ctx, "setup_endpoint_unrecorded",
			"integration", kind, "error", err,
			"detail", "the prior row could not be read, so nothing is written "+
				"over it; a later change of the public base URL will not be "+
				"reported for this surface until a pass records one")
		return
	}
	if current.Endpoint == base {
		return
	}
	current.Kind, current.Endpoint = kind, base
	if err := s.status.SaveIntegration(ctx, current); err != nil {
		log.WarnContext(ctx, "setup_endpoint_unrecorded",
			"integration", kind, "error", err,
			"detail", "a later change of the public base URL will not be "+
				"reported for this surface")
	}
}

// runByID serves GET /setup/integrations/{kind}/runs/{id}.
func (s *Service) runByID(w http.ResponseWriter, r *http.Request) {
	run, ok := s.passes.Get(r.PathValue("id"))
	if !ok || run.Kind != integration.Kind(r.PathValue("kind")) {
		httpjson.FailWith(w, http.StatusNotFound, codeRunNotFound, map[string]string{
			"hint": "a run is remembered by the node that executed it, and only " +
				"for the last few passes",
		})
		return
	}
	httpjson.Write(w, http.StatusOK, run)
}

// currentState is the row a surface already has, or the error that stopped
// this node reading it.
//
// THREE-VALUED, and every caller here reads the third value as a reason to
// write NOTHING. The rows carry attempts, the settled cadence, the address a
// surface was registered against and the last fault, and they are keyed per
// kind on the FLEET's store — so a read that failed is not evidence the
// surface has no row. Three copies of this each folded the failure into the
// zero State and then saved it, which is not a lost update but a blind
// overwrite: a two-second coordination blip erased everything a surface had
// ever recorded and reported it as freshly reconciled.
//
// An absent row IS the zero State, and that is a real answer: a surface
// nobody has reconciled has no row, which is why the miss is not an error.
func (s *Service) currentState(
	ctx context.Context, kind integration.Kind,
) (integration.State, error) {
	states, err := s.status.LoadIntegrations(ctx)
	if err != nil {
		return integration.State{}, err
	}
	for _, state := range states {
		if state.Kind == kind {
			return state, nil
		}
	}
	return integration.State{}, nil
}

// record folds a pass's outcome into the fleet's integration status.
func (s *Service) record(ctx context.Context, kind integration.Kind, run *setup.Run, passErr error) {
	if s.status == nil || run == nil {
		return
	}
	now := s.now()
	// The row this integration already has, so attempts and settled-at
	// carry across rather than resetting because a person pressed a
	// button.
	current, err := s.currentState(ctx, kind)
	if err != nil {
		// NOTHING IS WRITTEN. See [Service.currentState]: without the
		// prior row this would save a state built from an empty one,
		// erasing the attempts, the cadence and the address the surface
		// had. The pass itself happened and its work is durable; what is
		// lost is the record of it, which the loop's next tick rebuilds.
		log.WarnContext(ctx, "setup_status_unreadable",
			"integration", kind, "error", err,
			"detail", "the pass ran; its outcome is not recorded, and the "+
				"loop's next tick reports it")
		return
	}
	next, forget := integration.Observe(current, kind, run.Findings, passErr, now)
	// THE ADDRESS THIS PASS RAN AGAINST, and only where this pass is what
	// keeps that address current — the same three-way rule the loop applies,
	// through the same helper, so a row cannot mean one thing when a tick
	// wrote it and another when a button did.
	if company := s.company(); company != nil {
		integration.StampEndpoint(&next, kind, company.Integrations.WebhookBase(s.resolve))
	}
	if forget {
		if err := s.status.ForgetIntegration(ctx, kind); err != nil {
			log.WarnContext(ctx, "setup_status_not_forgotten",
				"integration", kind, "error", err)
		}
		return
	}
	// The SAME cadence the loop computes, so the next tick is due when the
	// loop would have made it due. A pass by hand changes what is true, not
	// how often the engine looks.
	next.NextAttemptAt = now.Add(integration.Schedule{}.WithDefaults().
		Next(next.Report, next.Attempts))
	if err := s.status.SaveIntegration(ctx, next); err != nil {
		// The pass happened and its work at the third-party app is durable. What is
		// lost is the record, so the loop re-runs a pass with nothing left
		// to do, which is the cheap failure.
		log.WarnContext(ctx, "setup_status_unrecorded", "integration", kind, "error", err)
	}
}

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().UTC()
}

// sinkFactory is how the service obtains a recorder for a pass.
type sinkFactory func(operator string) (provision.TokenSink, error)

// errorOrBusy names why a status write did not happen: the store's own failure
// when there was one, or the peer that holds the surface when there was not.
func errorOrBusy(err error) string {
	if err != nil {
		return err.Error()
	}
	return "another writer holds this surface"
}
