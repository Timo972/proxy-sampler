package api

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/ch"
	"github.com/timo972/proxy-sampler/internal/session"
)

const apiSamplePageSize = 50

var csvHeader = []string{
	"sample_seq", "sampled_at", "probe_index", "probe_ok", "probe_ip", "probe_rtt_ms", "primary_ip",
	"sample_probes_ok", "sample_probes_attempted", "rtt_min_ms", "rtt_med_ms", "rtt_max_ms", "ip_changed",
	"new_ips", "primary_category", "primary_risk", "error",
}

// SessionSamples returns one newest-first fixed-size page of grouped samples.
func (s *Server) SessionSamples(ctx context.Context, request openapi.SessionSamplesRequestObject) (openapi.SessionSamplesResponseObject, error) {
	if invalidOptionalRange(request.Params.From, request.Params.To) {
		return nil, invalidRequest()
	}
	page := 1
	if request.Params.Page != nil {
		page = *request.Params.Page
	}
	if page < 1 || s.store == nil || s.reader == nil {
		if page < 1 {
			return nil, invalidRequest()
		}
		return nil, dependencyUnavailable()
	}
	if _, err := s.store.SessionByID(ctx, request.Id); errors.Is(err, session.ErrNotFound) {
		return nil, notFound()
	} else if err != nil {
		return nil, dependencyUnavailable()
	}
	value, err := s.reader.Samples(ctx, request.Id, request.Params.From, request.Params.To, page)
	if errors.Is(err, ch.ErrInvalidPage) || errors.Is(err, ch.ErrInvalidRange) {
		return nil, invalidRequest()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}
	if value.Total > math.MaxInt64 {
		return nil, dependencyUnavailable()
	}
	result := openapi.SamplePage{
		Items: make([]openapi.SampleEvent, 0, len(value.Items)), Page: page,
		PageSize: apiSamplePageSize, Total: int64(value.Total),
	}
	for _, event := range value.Items {
		result.Items = append(result.Items, mapSampleEvent(event))
	}
	return openapi.SessionSamples200JSONResponse(result), nil
}

func mapSampleEvent(event ch.Event) openapi.SampleEvent {
	result := openapi.SampleEvent{
		SampleSeq: int64(event.SampleSeq), SampledAt: event.SampledAt,
		ProbesAttempted: int(event.ProbesAttempted), ProbesOk: int(event.ProbesOK),
		DistinctIps: int(event.DistinctIPs), IpChanged: event.IPChanged != 0, NewIps: int(event.NewIPs),
		RttMinMs: int(event.RTTMinMS), RttMedMs: int(event.RTTMedMS), RttMaxMs: int(event.RTTMaxMS),
		PrimaryCategory: event.PrimaryCategory, PrimaryRisk: int(event.PrimaryRisk), Error: event.Error,
		ProbeIps: make([]*string, len(event.ProbeIPs)), ProbeRttsMs: make([]int, len(event.ProbeRTTsMS)),
		ProbeOk: make([]bool, len(event.ProbeOK)),
	}
	if validIP(event.PrimaryIP) {
		primary := event.PrimaryIP.String()
		result.PrimaryIp = &primary
	}
	if event.EgressCountry != "" {
		country := event.EgressCountry
		result.EgressCountry = &country
	}
	for i, ip := range event.ProbeIPs {
		if i < len(event.ProbeOK) && event.ProbeOK[i] != 0 && validIP(ip) {
			value := ip.String()
			result.ProbeIps[i] = &value
		}
	}
	for i, value := range event.ProbeRTTsMS {
		result.ProbeRttsMs[i] = int(value)
	}
	for i, value := range event.ProbeOK {
		result.ProbeOk[i] = value != 0
	}
	return result
}

// ExportSessionCSV streams chronological per-probe rows through an io.Pipe.
func (s *Server) ExportSessionCSV(ctx context.Context, request openapi.ExportSessionCSVRequestObject) (openapi.ExportSessionCSVResponseObject, error) {
	if invalidOptionalRange(request.Params.From, request.Params.To) {
		return nil, invalidRequest()
	}
	if s.store == nil || s.reader == nil {
		return nil, dependencyUnavailable()
	}
	value, err := s.store.SessionByID(ctx, request.Id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, dependencyUnavailable()
	}

	body, started := s.csvStream(ctx, request.Id, request.Params.From, request.Params.To)
	if err := <-started; err != nil {
		_ = body.Close()
		return nil, dependencyUnavailable()
	}
	filename := sanitizedFilename(value.Name) + "-" + value.ID.String() + ".csv"
	disposition := `attachment; filename="` + filename + `"`
	return csvStreamResponse{body: body, contentDisposition: disposition}, nil
}

type csvStreamResponse struct {
	body               io.ReadCloser
	contentDisposition string
}

func (response csvStreamResponse) VisitExportSessionCSVResponse(w http.ResponseWriter) error {
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
			// Once CSV headers or rows have been sent, HTTP cannot change to a
			// structured error response. End the truncated stream without
			// appending JSON to an otherwise valid CSV prefix.
			return nil
		}
	}
}

func (s *Server) csvStream(ctx context.Context, id openapi.SessionID, from, to *time.Time) (*io.PipeReader, <-chan error) {
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
			return csvWriter.Write(csvHeader)
		}
		err := s.reader.StreamSamples(ctx, id, from, to, func(event ch.Event) error {
			announce(nil)
			if err := writeHeader(); err != nil {
				return err
			}
			if err := writeCSVEvent(csvWriter, event); err != nil {
				return err
			}
			csvWriter.Flush()
			return csvWriter.Error()
		})
		if !announced && err != nil {
			announce(err)
			_ = writer.CloseWithError(err)
			return
		}
		announce(nil)
		if err == nil {
			err = writeHeader()
		}
		csvWriter.Flush()
		if err == nil {
			err = csvWriter.Error()
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader, started
}

func writeCSVEvent(writer *csv.Writer, event ch.Event) error {
	primary := ""
	if validIP(event.PrimaryIP) {
		primary = event.PrimaryIP.String()
	}
	for i := 0; i < int(event.ProbesAttempted); i++ {
		ok := i < len(event.ProbeOK) && event.ProbeOK[i] != 0
		ip := ""
		if ok && i < len(event.ProbeIPs) && validIP(event.ProbeIPs[i]) {
			ip = event.ProbeIPs[i].String()
		}
		rtt := uint32(0)
		if i < len(event.ProbeRTTsMS) {
			rtt = event.ProbeRTTsMS[i]
		}
		row := []string{
			strconv.FormatUint(uint64(event.SampleSeq), 10), event.SampledAt.UTC().Format(time.RFC3339Nano), strconv.Itoa(i),
			strconv.FormatBool(ok), ip, strconv.FormatUint(uint64(rtt), 10), primary,
			strconv.Itoa(int(event.ProbesOK)), strconv.Itoa(int(event.ProbesAttempted)),
			strconv.FormatUint(uint64(event.RTTMinMS), 10), strconv.FormatUint(uint64(event.RTTMedMS), 10), strconv.FormatUint(uint64(event.RTTMaxMS), 10),
			strconv.FormatBool(event.IPChanged != 0), strconv.Itoa(int(event.NewIPs)), event.PrimaryCategory,
			strconv.Itoa(int(event.PrimaryRisk)), event.Error,
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func sanitizedFilename(name string) string {
	name = strings.TrimSpace(name)
	var result strings.Builder
	invalidRun := false
	for _, char := range name {
		allowed := char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-'
		if allowed {
			if invalidRun {
				result.WriteByte('-')
				invalidRun = false
			}
			result.WriteRune(char)
		} else {
			invalidRun = true
		}
	}
	if invalidRun {
		result.WriteByte('-')
	}
	if result.Len() == 0 {
		return "session"
	}
	return result.String()
}

func invalidOptionalRange(from, to *time.Time) bool {
	return from != nil && to != nil && from.After(*to)
}

func validIP(ip net.IP) bool { return ip != nil && !ip.IsUnspecified() }
