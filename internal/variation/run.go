package variation

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/timo972/proxy-sampler/internal/session"
)

// ErrRunNotFound is returned when a run row does not exist.
var ErrRunNotFound = errors.New("variation run not found")

// RunStatus is a run's status, derived from its children.
type RunStatus string

const (
	RunRunning  RunStatus = "running"
	RunStopped  RunStatus = "stopped"
	RunFinished RunStatus = "finished"
)

// Run is a durable variation run owning many child sessions.
type Run struct {
	ID                 uuid.UUID
	Name               string
	TemplateCiphertext []byte
	TemplateNonce      []byte
	TemplateDisplay    string
	Axes               json.RawMessage
	CreatedAt          time.Time
}

// ChildSession pairs a session with its resolved variant params for insertion.
type ChildSession struct {
	Session session.Session
	Params  json.RawMessage
	CellKey string
}

// RunSummary is a run plus its rolled-up counts and derived status.
type RunSummary struct {
	Run
	VariantCount int
	DistinctIPs  int
	Status       RunStatus
}

// VariantSession is one child session's summary within a run.
type VariantSession struct {
	SessionID uuid.UUID
	Name      string
	Params    json.RawMessage
	CellKey   string
	// TargetCountry is the manually declared target country (ISO 3166-1
	// alpha-2). When set it overrides any country axis param for honor
	// evaluation; runs whose template encodes no country axis have no other
	// requested-country signal.
	TargetCountry string
	Status        session.Status
	Snapshot      session.Snapshot
}

// IPObservation is one child session observing one exit IP, with reputation.
type IPObservation struct {
	SessionID  uuid.UUID
	IP         netip.Addr
	HitCount   int64
	FirstSeen  time.Time
	LastSeen   time.Time
	Reputation *session.Reputation
}

// Store persists and reads variation runs and their aggregates.
type Store interface {
	CreateRun(ctx context.Context, run Run, children []ChildSession) error
	Runs(ctx context.Context) ([]RunSummary, error)
	RunByID(ctx context.Context, id uuid.UUID) (RunSummary, error)
	RunSessions(ctx context.Context, id uuid.UUID) ([]VariantSession, error)
	// RunIPObservations returns at most limit observations (most recent first)
	// so a report over an unbounded pool run cannot exhaust memory.
	RunIPObservations(ctx context.Context, id uuid.UUID, limit int) ([]IPObservation, error)
	// StreamPoolIPs visits each distinct exit IP of a run once, deduped and
	// aggregated server-side, so a large export never holds the whole pool in
	// memory. Rows arrive in ascending IP order.
	StreamPoolIPs(ctx context.Context, id uuid.UUID, visit func(IPRow) error) error
	// RenameRun changes a run's name and, in the same transaction, the names of
	// the child sessions that still carry the name VariantName generated from
	// the run's previous name. A child renamed by hand no longer matches and is
	// left alone.
	RenameRun(ctx context.Context, id uuid.UUID, name string) error
	DeleteRun(ctx context.Context, id uuid.UUID) error
}
