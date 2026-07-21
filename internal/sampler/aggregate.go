package sampler

import (
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxUInt8Value  = 1<<8 - 1
	maxSampleError = 2 << 10
)

// Sample is the deterministic aggregate of one ordered set of probes.
type Sample struct {
	ProbesAttempted int
	ProbesOK        int
	PrimaryIP       netip.Addr
	DistinctIPs     int
	IPChanged       bool
	NewIPs          int
	RTTMin          time.Duration
	RTTMed          time.Duration
	RTTMax          time.Duration
	EgressCountry   string
	ProbeIPs        []netip.Addr
	ProbeRTTs       []time.Duration
	ProbeOK         []bool
	IPHits          map[netip.Addr]int
	Error           string
}

// Aggregate folds ordered probe results into a ClickHouse-ready sample.
func Aggregate(results []ProbeResult, previous netip.Addr, seen map[netip.Addr]struct{}) Sample {
	if len(results) > maxUInt8Value {
		results = results[:maxUInt8Value]
	}
	sample := Sample{
		ProbesAttempted: clampUInt8(len(results)),
		ProbeIPs:        make([]netip.Addr, len(results)),
		ProbeRTTs:       make([]time.Duration, len(results)),
		ProbeOK:         make([]bool, len(results)),
		IPHits:          make(map[netip.Addr]int),
	}
	countries := make(map[netip.Addr]string)
	rtts := make([]time.Duration, 0, len(results))
	errorsInOrder := make([]string, 0, len(results))

	for index, result := range results {
		if result.Err != nil || !result.IP.IsValid() {
			if result.Err != nil {
				errorsInOrder = append(errorsInOrder, result.Err.Error())
			} else {
				errorsInOrder = append(errorsInOrder, "probe returned invalid IP")
			}
			continue
		}

		sample.ProbeOK[index] = true
		sample.ProbeIPs[index] = result.IP
		sample.ProbeRTTs[index] = result.RTT
		sample.IPHits[result.IP]++
		if _, exists := countries[result.IP]; !exists {
			countries[result.IP] = result.Country
		}
		rtts = append(rtts, result.RTT)
	}

	sample.ProbesOK = clampUInt8(len(rtts))
	sample.DistinctIPs = clampUInt8(len(sample.IPHits))
	for ip := range sample.IPHits {
		if _, exists := seen[ip]; !exists {
			sample.NewIPs = clampUInt8(sample.NewIPs + 1)
		}
	}

	bestHits := 0
	for index, ok := range sample.ProbeOK {
		if !ok {
			continue
		}
		ip := sample.ProbeIPs[index]
		if hits := sample.IPHits[ip]; hits > bestHits {
			sample.PrimaryIP = ip
			bestHits = hits
		}
	}
	if sample.PrimaryIP.IsValid() {
		sample.EgressCountry = countries[sample.PrimaryIP]
	}
	sample.IPChanged = previous.IsValid() && sample.PrimaryIP.IsValid() && previous != sample.PrimaryIP

	if len(rtts) == 0 {
		sample.Error = joinProbeErrors(errorsInOrder)
		return sample
	}
	slices.Sort(rtts)
	sample.RTTMin = rtts[0]
	sample.RTTMax = rtts[len(rtts)-1]
	middle := len(rtts) / 2
	if len(rtts)%2 == 0 {
		sample.RTTMed = rtts[middle-1] + (rtts[middle]-rtts[middle-1])/2
	} else {
		sample.RTTMed = rtts[middle]
	}
	return sample
}

func clampUInt8(value int) int {
	if value > maxUInt8Value {
		return maxUInt8Value
	}
	return value
}

func joinProbeErrors(messages []string) string {
	unique := make([]string, 0, len(messages))
	seen := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		if _, exists := seen[message]; exists {
			continue
		}
		seen[message] = struct{}{}
		unique = append(unique, message)
	}
	joined := strings.Join(unique, "; ")
	if len(joined) <= maxSampleError {
		return joined
	}
	joined = joined[:maxSampleError]
	for !utf8.ValidString(joined) {
		joined = joined[:len(joined)-1]
	}
	return joined
}
