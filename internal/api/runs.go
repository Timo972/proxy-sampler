package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
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
	if body.Template == nil || strings.TrimSpace(*body.Template) == "" {
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

	for _, child := range children {
		if err := s.control.Start(ctx, child.Session.ID); err != nil {
			cleanupContext := context.WithoutCancel(ctx)
			_ = s.store.Stop(cleanupContext, child.Session.ID, s.now().UTC())
		}
	}

	summary, err := s.runStore.RunByID(ctx, run.ID)
	if err != nil {
		return nil, internalError()
	}
	return openapi.CreateRun201JSONResponse(mapRun(summary)), nil
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

// ExportRunCSV streams the deduped pool IP list with reputation columns.
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
	variants, err := s.runStore.RunSessions(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	observations, err := s.runStore.RunIPObservations(ctx, request.Id)
	if err != nil {
		return nil, dependencyUnavailable()
	}
	pool := variation.BuildPoolReport(variants, observations)

	data := runIPCSV(pool.IPs)
	filename := sanitizedFilename(summary.Name) + "-" + summary.ID.String() + "-pool.csv"
	disposition := `attachment; filename="` + filename + `"`
	return openapi.ExportRunCSV200TextcsvResponse{
		Body:          bytes.NewReader(data),
		Headers:       openapi.ExportRunCSV200ResponseHeaders{ContentDisposition: &disposition},
		ContentLength: int64(len(data)),
	}, nil
}

var runCSVHeader = []string{"ip", "category", "country", "isp", "asn", "risk_score", "greynoise_class", "dnsbl_listed", "dnsbl_hits", "hit_count"}

// runIPCSV renders the deduped pool IP list as CSV bytes. The pool is bounded
// by MAX_VARIANTS_PER_RUN x distinct IPs, small enough to buffer in memory.
func runIPCSV(rows []variation.IPRow) []byte {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	_ = writer.Write(runCSVHeader)
	for _, row := range rows {
		risk := ""
		if row.RiskScore != nil {
			risk = strconv.Itoa(*row.RiskScore)
		}
		_ = writer.Write([]string{
			row.IP, row.Category, row.Country, row.ISP, row.ASN, risk, row.GreyNoiseClass,
			strconv.FormatBool(row.DNSBLListed), strings.Join(row.DNSBLHits, "|"),
			strconv.FormatInt(row.HitCount, 10),
		})
	}
	writer.Flush()
	return buf.Bytes()
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
