package api

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
	"github.com/timo972/proxy-sampler/internal/session"
	"github.com/timo972/proxy-sampler/internal/variation"
)

// EditSession applies the mutable properties of a session. Only the display
// name is editable today; the request shape carries every property as optional
// so further fields can join it without breaking existing clients.
func (s *Server) EditSession(ctx context.Context, request openapi.EditSessionRequestObject) (openapi.EditSessionResponseObject, error) {
	if request.Body == nil {
		return nil, invalidRequest()
	}
	name, err := editedName(request.Body.Name)
	if err != nil {
		return nil, err
	}

	if err := s.store.Rename(ctx, request.Id, name); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return nil, notFound()
		}
		return nil, internalError()
	}
	value, err := s.store.SessionByID(ctx, request.Id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return nil, notFound()
		}
		return nil, internalError()
	}
	return openapi.EditSession200JSONResponse(mapSession(value)), nil
}

// EditRun applies the mutable properties of a run. Renaming a run also renames
// the child sessions that still carry their generated name, so a run and its
// variants stay consistent while any variant renamed by hand is preserved.
func (s *Server) EditRun(ctx context.Context, request openapi.EditRunRequestObject) (openapi.EditRunResponseObject, error) {
	if s.runStore == nil {
		return nil, internalError()
	}
	if request.Body == nil {
		return nil, invalidRequest()
	}
	name, err := editedName(request.Body.Name)
	if err != nil {
		return nil, err
	}

	if err := s.runStore.RenameRun(ctx, request.Id, name); err != nil {
		if errors.Is(err, variation.ErrRunNotFound) {
			return nil, notFound()
		}
		return nil, internalError()
	}
	summary, err := s.runStore.RunByID(ctx, request.Id)
	if err != nil {
		if errors.Is(err, variation.ErrRunNotFound) {
			return nil, notFound()
		}
		return nil, internalError()
	}
	return openapi.EditRun200JSONResponse(mapRun(summary)), nil
}

// editedName validates the one property an edit request can currently set. A
// nil pointer means the client sent no editable property at all, which is a
// bad request rather than a silent no-op.
func editedName(raw *string) (string, error) {
	if raw == nil {
		return "", invalidRequest()
	}
	name := strings.TrimSpace(*raw)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", invalidRequest()
	}
	return name, nil
}
