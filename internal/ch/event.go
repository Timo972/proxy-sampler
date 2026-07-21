// Package ch persists sample events and reads ClickHouse-backed reports.
package ch

import (
	"net"
	"time"

	"github.com/google/uuid"
)

// Event is one aggregated sampling tick and its per-probe observations.
type Event struct {
	SessionID       uuid.UUID
	SampledAt       time.Time
	SampleSeq       uint32
	ProbesAttempted uint8
	ProbesOK        uint8
	PrimaryIP       net.IP
	DistinctIPs     uint8
	IPChanged       uint8
	NewIPs          uint8
	RTTMinMS        uint32
	RTTMedMS        uint32
	RTTMaxMS        uint32
	EgressCountry   string
	PrimaryCategory string
	PrimaryRisk     uint8
	ProbeIPs        []net.IP
	ProbeRTTsMS     []uint32
	ProbeOK         []uint8
	Error           string
}

// SamplePage is a newest-first page of sample events.
type SamplePage struct {
	Items    []Event
	Page     int
	PageSize int
	Total    uint64
}

// SeriesPoint is a time-bucketed report point.
type SeriesPoint struct {
	At                time.Time
	SuccessRate       float64
	LatencyP50MS      float64
	LatencyP95MS      float64
	DistinctPerSample float64
	IPChanges         uint64
	Mobile            uint64
	Residential       uint64
	Datacenter        uint64
	Unknown           uint64
}

// Hold is a contiguous successful run on one primary IP.
type Hold struct {
	IP              string
	StartedAt       time.Time
	EndedAt         time.Time
	Samples         uint32
	DurationSeconds int64
}

// Rotation is a transition between successful primary-IP runs.
type Rotation struct {
	At                   time.Time
	FromIP               string
	ToIP                 string
	SincePreviousSeconds int64
}

// Stickiness summarizes successful primary-IP holds and rotations.
type Stickiness struct {
	Holds              []Hold
	Rotations          []Rotation
	AverageHoldSeconds float64
	MedianHoldSeconds  float64
}

// GrowthPoint is cumulative distinct-IP pool growth at one sample.
type GrowthPoint struct {
	At          time.Time
	DistinctIPs uint64
}
