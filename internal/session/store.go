package session

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound   = errors.New("session not found")
	ErrNotRunning = errors.New("session is not running")
)

type Store interface {
	Create(ctx context.Context, s Session) (Session, error)
	Sessions(ctx context.Context) ([]Session, error)
	SessionByID(ctx context.Context, id uuid.UUID) (Session, error)
	RunningSessions(ctx context.Context) ([]Session, error)
	Stop(ctx context.Context, id uuid.UUID, at time.Time) error
	Finish(ctx context.Context, id uuid.UUID, at time.Time) error
	Delete(ctx context.Context, id uuid.UUID) error
	SaveTick(ctx context.Context, id uuid.UUID, snapshot Snapshot, hits []IPHit) error
	SessionIPs(ctx context.Context, id uuid.UUID) ([]IPRecord, error)
	ReputationByIP(ctx context.Context, ip netip.Addr) (Reputation, bool, error)
	SaveReputation(ctx context.Context, reputation Reputation) error
}
