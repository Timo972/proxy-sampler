// Package session defines the sampler's persistence-safe session domain.
package session

import (
	"encoding/json"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type Mode string

const (
	ModeSticky Mode = "sticky"
	ModePool   Mode = "pool"
)

type Status string

const (
	StatusRunning  Status = "running"
	StatusStopped  Status = "stopped"
	StatusFinished Status = "finished"
)

type Session struct {
	ID              uuid.UUID
	Name            string
	ProxyCiphertext []byte
	ProxyNonce      []byte
	ProxyDisplay    string
	Mode            Mode
	Cadence         time.Duration
	ProbesPerSample int
	ProbeTarget     string
	DialTimeout     time.Duration
	MaxSamples      *int
	MaxDuration     *time.Duration
	Status          Status
	Snapshot        Snapshot
	CreatedAt       time.Time
	StartedAt       *time.Time
	StoppedAt       *time.Time
}

type Snapshot struct {
	SamplesTaken  int
	ProbesOK      int64
	ProbesTotal   int64
	DistinctIPs   int
	LastSampleAt  *time.Time
	LastPrimaryIP netip.Addr
	LastCategory  string
	LastRTT       *time.Duration
	LastError     string
}

type IPHit struct {
	IP     netip.Addr
	SeenAt time.Time
	Hits   int64
}

type IPRecord struct {
	IP         netip.Addr
	FirstSeen  time.Time
	LastSeen   time.Time
	HitCount   int64
	Reputation *Reputation
}

type Reputation struct {
	IP              netip.Addr
	Country         string
	Region          string
	City            string
	ISP             string
	ASN             string
	IsMobile        *bool
	IPAPIProxy      *bool
	IPAPIHosting    *bool
	ProxyCheckType  string
	ProxyCheckProxy *bool
	RiskScore       *int
	GreyNoiseClass  string
	SFSAppears      *bool
	SFSFrequency    *int
	DNSBLListed     *bool
	DNSBLHits       []string
	Category        string
	Raw             json.RawMessage
	FirstSeen       time.Time
	RefreshedAt     time.Time
}
