package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/proxydial"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// CreateRun parses a proxy-URL template plus its parameter axes, expands the
// cartesian product into concrete proxy variants (capped at maxVariants),
// persists the run and its child sessions, and starts every child worker.
func (s *Server) CreateRun(ctx context.Context, request openapi.CreateRunRequestObject) (openapi.CreateRunResponseObject, error) {
	if request.Body == nil {
		return nil, invalidRequest()
	}
	body := request.Body
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return nil, invalidRequest()
	}
	if body.Mode != openapi.CreateRunRequestModeSticky && body.Mode != openapi.CreateRunRequestModePool {
		return nil, invalidRequest()
	}
	if !validPersistedInteger(body.CadenceSeconds, 1) {
		return nil, invalidRequest()
	}
	// The template is rendered once per variant (and each rendering is then
	// encrypted), so an oversized template multiplies into a large allocation
	// under the variant cap. A proxy URL is small; cap the template well below
	// the request-body limit to bound the amplification.
	if body.Template == nil || strings.TrimSpace(*body.Template) == "" || len(*body.Template) > maxTemplateBytes {
		return nil, invalidRequest()
	}

	probes := defaultProbes(openapi.CreateSessionRequestMode(body.Mode))
	if body.ProbesPerSample != nil {
		probes = *body.ProbesPerSample
	}
	if probes < 1 || probes > 255 {
		return nil, invalidRequest()
	}
	probeTarget := s.defaults.ProbeTarget
	if body.ProbeTarget != nil {
		probeTarget = *body.ProbeTarget
	}
	if !validProbeTarget(probeTarget) {
		return nil, invalidRequest()
	}
	dialTimeout := s.defaults.DialTimeout
	if body.DialTimeoutMs != nil {
		if !validPersistedInteger(*body.DialTimeoutMs, 100) {
			return nil, invalidRequest()
		}
		dialTimeout = time.Duration(*body.DialTimeoutMs) * time.Millisecond
	}
	if dialTimeout < 100*time.Millisecond || dialTimeout > time.Duration(maxPersistedInteger)*time.Millisecond ||
		!validOptionalPersistedInteger(body.MaxSamples) || !validOptionalPersistedInteger(body.MaxDurationSeconds) {
		return nil, invalidRequest()
	}

	if s.runStore == nil || s.cipher == nil || s.store == nil || s.control == nil {
		return nil, internalError()
	}

	template, err := variation.ParseTemplate(*body.Template)
	if err != nil {
		return nil, invalidRequest()
	}
	axes, err := mapAxes(body.Axes)
	if err != nil {
		return nil, invalidRequest()
	}
	variants, err := variation.Expand(template, axes, variation.DefaultRandString, s.maxVariants)
	if err != nil {
		return nil, invalidRequest()
	}
	if len(variants) == 0 {
		return nil, invalidRequest()
	}

	templateCiphertext, templateNonce, err := s.cipher.Encrypt(*body.Template)
	if err != nil {
		return nil, internalError()
	}
	templateDisplay, err := proxydial.Display(*body.Template)
	if err != nil {
		templateDisplay = redactTemplate(*body.Template)
	}
	axesJSON, err := json.Marshal(body.Axes)
	if err != nil {
		return nil, internalError()
	}

	now := s.now().UTC()
	run := variation.Run{
		ID: uuid.New(), Name: name, TemplateCiphertext: templateCiphertext, TemplateNonce: templateNonce,
		TemplateDisplay: templateDisplay, Axes: axesJSON, CreatedAt: now,
	}

	children := make([]variation.ChildSession, 0, len(variants))
	for _, variant := range variants {
		variantDisplay, err := proxydial.Display(variant.URL)
		if err != nil {
			return nil, invalidRequest()
		}
		ciphertext, nonce, err := s.cipher.Encrypt(variant.URL)
		if err != nil {
			return nil, internalError()
		}
		paramsJSON, err := json.Marshal(variant.Params)
		if err != nil {
			return nil, internalError()
		}
		child := session.Session{
			ID: uuid.New(), Name: variantName(name, variant.Params), ProxyCiphertext: ciphertext, ProxyNonce: nonce,
			ProxyDisplay: variantDisplay, Mode: session.Mode(body.Mode),
			Cadence: time.Duration(body.CadenceSeconds) * time.Second, ProbesPerSample: probes,
			ProbeTarget: probeTarget, DialTimeout: dialTimeout, MaxSamples: cloneInt(body.MaxSamples),
			MaxDuration: secondsPointer(body.MaxDurationSeconds), Status: session.StatusRunning,
			CreatedAt: now, StartedAt: timePointer(now),
		}
		children = append(children, variation.ChildSession{Session: child, Params: paramsJSON, CellKey: variant.CellKey})
	}

	if err := s.runStore.CreateRun(ctx, run, children); err != nil {
		return nil, internalError()
	}

	// Every child shares one proxy template, so a Start failure (e.g. an
	// unsupported scheme, an unreachable gateway) is systemic rather than
	// per-variant. Roll the whole run back instead of returning a 201 for a
	// run with dead or missing variants.
	var startErr error
	for _, child := range children {
		if err := s.control.Start(ctx, child.Session.ID); err != nil {
			startErr = err
			break
		}
	}
	if startErr != nil {
		cleanup := context.WithoutCancel(ctx)
		cleanupConfirmed := true
		for _, child := range children {
			// Delete stops the worker (if it started), crosses the ClickHouse
			// flush barrier, and removes the child row. A child that never
			// started has nothing to stop.
			if err := s.control.Delete(cleanup, child.Session.ID); err != nil && !errors.Is(err, session.ErrNotFound) {
				cleanupConfirmed = false
			}
		}
		// Only delete the run row once every child is confirmed gone. Deleting
		// it earlier would cascade-delete child rows out from under a worker
		// whose cleanup failed, orphaning it. If cleanup is unconfirmed we
		// leave the run durable and consistent — the operator can DELETE it via
		// the API — which is strictly safer than an unmanageable orphan worker.
		if cleanupConfirmed {
			_ = s.runStore.DeleteRun(cleanup, run.ID)
		}
		return nil, internalError()
	}

	// Build the response from what we already know rather than re-reading. A
	// failed post-start read would return 500 while the run and its workers
	// stay live, and — with no idempotency key — a client retry would create a
	// duplicate live run. A freshly created run has every child running, no
	// samples yet, and a known variant count.
	summary := variation.RunSummary{
		Run:          run,
		VariantCount: len(children),
		DistinctIPs:  0,
		Status:       variation.RunRunning,
	}
	return openapi.CreateRun201JSONResponse(mapRun(summary)), nil
}

// RunConfig exposes run-related client configuration, currently the server's
// variant cap, so the UI can validate against the real limit instead of a
// hard-coded default.
func (s *Server) RunConfig(_ context.Context, _ openapi.RunConfigRequestObject) (openapi.RunConfigResponseObject, error) {
	limit := s.maxVariants
	if limit <= 0 {
		limit = 128
	}
	return openapi.RunConfig200JSONResponse{MaxVariantsPerRun: limit}, nil
}

// ListRuns returns every durable run summary.
func (s *Server) ListRuns(ctx context.Context, _ openapi.ListRunsRequestObject) (openapi.ListRunsResponseObject, error) {
	if s.runStore == nil {
		return nil, internalError()
	}
	summaries, err := s.runStore.Runs(ctx)
	if err != nil {
		return nil, internalError()
	}
	result := make(openapi.ListRuns200JSONResponse, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, mapRun(summary))
	}
	return result, nil
}

// RunByID returns one run summary plus its child variant summaries.
func (s *Server) RunByID(ctx context.Context, request openapi.RunByIDRequestObject) (openapi.RunByIDResponseObject, error) {
	if s.runStore == nil {
		return nil, internalError()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, internalError()
	}
	sessions, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	variants := make([]openapi.VariantSummary, 0, len(sessions))
	for _, variantSession := range sessions {
		variants = append(variants, mapVariant(variantSession))
	}
	return openapi.RunByID200JSONResponse(openapi.RunDetail{Run: mapRun(summary), Variants: variants}), nil
}

// StopRun stops every running child of a run. Children already stopped are
// skipped; the refreshed run summary is returned.
func (s *Server) StopRun(ctx context.Context, request openapi.StopRunRequestObject) (openapi.StopRunResponseObject, error) {
	if err := s.fanOut(ctx, request.Id, s.control.Stop, session.ErrNotRunning); err != nil {
		return nil, err
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	return openapi.StopRun200JSONResponse(mapRun(summary)), nil
}

// ReenableRun re-enables every stopped/finished child of a run.
func (s *Server) ReenableRun(ctx context.Context, request openapi.ReenableRunRequestObject) (openapi.ReenableRunResponseObject, error) {
	if err := s.fanOut(ctx, request.Id, s.control.Reenable, session.ErrAlreadyRunning); err != nil {
		return nil, err
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	return openapi.ReenableRun200JSONResponse(mapRun(summary)), nil
}

// DeleteRun deletes every child (crossing the ClickHouse flush barrier per
// child) and then the run row.
func (s *Server) DeleteRun(ctx context.Context, request openapi.DeleteRunRequestObject) (openapi.DeleteRunResponseObject, error) {
	if s.runStore == nil || s.control == nil {
		return nil, internalError()
	}
	if _, err := s.runStore.RunByID(ctx, request.Id); errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	} else if err != nil {
		return nil, internalError()
	}
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, internalError()
	}
	for _, v := range variants {
		if err := s.control.Delete(ctx, v.SessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return nil, internalError()
		}
	}
	if err := s.runStore.DeleteRun(ctx, request.Id); err != nil {
		return nil, internalError()
	}
	return openapi.DeleteRun204Response{}, nil
}

// fanOut applies op to each child, treating idempotentSkip as success.
func (s *Server) fanOut(ctx context.Context, runID uuid.UUID, op func(context.Context, uuid.UUID) error, idempotentSkip error) error {
	if s.runStore == nil || s.control == nil {
		return internalError()
	}
	if _, err := s.runStore.RunByID(ctx, runID); errors.Is(err, variation.ErrRunNotFound) {
		return notFound()
	} else if err != nil {
		return internalError()
	}
	variants, err := s.runStore.RunSessions(ctx, runID)
	if err != nil {
		return internalError()
	}
	for _, v := range variants {
		err := op(ctx, v.SessionID)
		if err == nil || errors.Is(err, idempotentSkip) || errors.Is(err, session.ErrNotFound) {
			continue
		}
		return internalError()
	}
	return nil
}

// ExportRunCSV streams the deduped pool IP list with reputation columns. The
// rows are deduplicated server-side and streamed through a pipe so a run with
// an unbounded number of distinct exit IPs cannot exhaust service memory.
func (s *Server) ExportRunCSV(ctx context.Context, request openapi.ExportRunCSVRequestObject) (openapi.ExportRunCSVResponseObject, error) {
	if s.runStore == nil {
		return nil, dependencyUnavailable()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if errors.Is(err, variation.ErrRunNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}

	body, started := s.runCSVStream(ctx, request.Id)
	if err := <-started; err != nil {
		_ = body.Close()
		return nil, dependencyUnavailable()
	}
	filename := sanitizedFilename(summary.Name) + "-" + summary.ID.String() + "-pool.csv"
	disposition := `attachment; filename="` + filename + `"`
	return runCSVStreamResponse{body: body, contentDisposition: disposition}, nil
}

var runCSVHeader = []string{"ip", "category", "country", "isp", "asn", "risk_score", "greynoise_class", "dnsbl_listed", "dnsbl_hits", "hit_count"}

// runCSVStream writes the deduped pool CSV to a pipe. It announces success on
// the started channel only once the pool query has actually produced its first
// row or completed cleanly — not merely after buffering the header — so a query
// that fails before yielding any row is reported to the caller (which turns it
// into a 503) instead of emitting a valid-looking header-only CSV.
func (s *Server) runCSVStream(ctx context.Context, id openapi.RunID) (*io.PipeReader, <-chan error) {
	reader, writer := io.Pipe()
	started := make(chan error, 1)
	go func() {
		csvWriter := csv.NewWriter(writer)
		announced := false
		headerWritten := false
		announce := func(err error) {
			if !announced {
				announced = true
				started <- err
			}
		}
		writeHeader := func() error {
			if headerWritten {
				return nil
			}
			headerWritten = true
			return csvWriter.Write(runCSVHeader)
		}
		streamErr := s.runStore.StreamPoolIPs(ctx, id, func(row variation.IPRow) error {
			announce(nil)
			if err := writeHeader(); err != nil {
				return err
			}
			if err := writeRunIPRow(csvWriter, row); err != nil {
				return err
			}
			csvWriter.Flush()
			return csvWriter.Error()
		})
		// The query failed before yielding any row: report it so the handler
		// can still return a structured error.
		if !announced && streamErr != nil {
			announce(streamErr)
			_ = writer.CloseWithError(streamErr)
			return
		}
		announce(nil)
		if streamErr == nil {
			streamErr = writeHeader() // empty pool still gets a header row
		}
		csvWriter.Flush()
		if streamErr == nil {
			streamErr = csvWriter.Error()
		}
		if streamErr != nil {
			_ = writer.CloseWithError(streamErr)
			return
		}
		_ = writer.Close()
	}()
	return reader, started
}

func writeRunIPRow(writer *csv.Writer, row variation.IPRow) error {
	risk := ""
	if row.RiskScore != nil {
		risk = strconv.Itoa(*row.RiskScore)
	}
	return writer.Write([]string{
		row.IP, row.Category, row.Country, row.ISP, row.ASN, risk, row.GreyNoiseClass,
		strconv.FormatBool(row.DNSBLListed), strings.Join(row.DNSBLHits, "|"),
		strconv.FormatInt(row.HitCount, 10),
	})
}

type runCSVStreamResponse struct {
	body               *io.PipeReader
	contentDisposition string
}

func (response runCSVStreamResponse) VisitExportRunCSVResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", response.contentDisposition)
	w.WriteHeader(http.StatusOK)
	defer response.body.Close()
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := response.body.Read(buffer)
		if count > 0 {
			if _, err := w.Write(buffer[:count]); err != nil {
				return err
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			// Headers/rows already sent: end the truncated stream rather than
			// appending a JSON error to an otherwise valid CSV prefix.
			return nil
		}
	}
}

// mapAxes converts generated OpenAPI axis specs into the variation package's
// domain type, defaulting optional pointer fields to their zero values.
func mapAxes(axes map[string]openapi.AxisSpec) (map[string]variation.AxisSpec, error) {
	result := make(map[string]variation.AxisSpec, len(axes))
	for name, spec := range axes {
		domain := variation.AxisSpec{Kind: variation.AxisKind(spec.Kind)}
		if spec.Values != nil {
			domain.Values = *spec.Values
		}
		// A range axis needs both endpoints. Nil pointers would otherwise
		// silently become 0..0 (a live one-variant run) instead of a 400.
		if domain.Kind == variation.AxisRange && (spec.From == nil || spec.To == nil) {
			return nil, fmt.Errorf("range axis %q requires from and to", name)
		}
		if spec.From != nil {
			domain.From = *spec.From
		}
		if spec.To != nil {
			domain.To = *spec.To
		}
		if spec.Count != nil {
			domain.Count = *spec.Count
		}
		if spec.Length != nil {
			domain.Length = *spec.Length
		}
		result[name] = domain
	}
	return result, nil
}

func mapRun(summary variation.RunSummary) openapi.Run {
	return openapi.Run{
		Id: summary.ID, Name: summary.Name, TemplateDisplay: summary.TemplateDisplay,
		Status: openapi.RunStatus(summary.Status), VariantCount: summary.VariantCount,
		DistinctIps: summary.DistinctIPs, CreatedAt: summary.CreatedAt,
	}
}

func mapVariant(v variation.VariantSession) openapi.VariantSummary {
	params := map[string]string{}
	_ = json.Unmarshal(v.Params, &params)
	return openapi.VariantSummary{
		SessionId: v.SessionID, Name: v.Name, CellKey: v.CellKey, Params: params,
		Status: openapi.VariantSummaryStatus(v.Status), SamplesTaken: v.Snapshot.SamplesTaken,
		DistinctIps: v.Snapshot.DistinctIPs,
	}
}

// variantName combines the run name with a compact, deterministic suffix
// built from the variant's sorted params so child sessions are distinguishable.
func variantName(runName string, params map[string]string) string {
	if len(params) == 0 {
		return runName
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return runName + " (" + strings.Join(parts, ",") + ")"
}

// redactTemplate is a fallback display value for templates proxydial.Display
// cannot parse directly — most commonly a placeholder inside the userinfo
// section (e.g. "u-cc-{country}:pw@host:port"), which net/url rejects
// outright because "{" and "}" are not valid userinfo characters. It strips
// the userinfo section and retries Display so template_display never leaks
// credentials or unparsed placeholder syntax; if that still fails, it falls
// back to a fully redacted placeholder.
func redactTemplate(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd == -1 {
		return "***"
	}
	rest := raw[schemeEnd+3:]
	if atIdx := strings.LastIndex(rest, "@"); atIdx != -1 {
		stripped := raw[:schemeEnd+3] + rest[atIdx+1:]
		if display, err := proxydial.Display(stripped); err == nil {
			return display
		}
	}
	return "***"
}
