package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/msrsiddik/apicorex-sync/internal/plugin"
	"github.com/msrsiddik/apicorex-sync/internal/syncdb"
	"github.com/oaswrap/spec/option"
)

const (
	defaultPullLimit = 500
	maxPullLimit     = 1000
	maxPushBatch     = 1000 // max changes per push request
)

// --- Request / Response types ---

type PushChange struct {
	ChangeID   string          `json:"change_id"   example:"chg-uuid"`
	Collection string          `json:"collection"  example:"notes"`
	RecordID   string          `json:"record_id"   example:"note-uuid"`
	Deleted    bool            `json:"deleted"     example:"false"`
	Payload    json.RawMessage `json:"payload"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type PushRequest struct {
	Changes []PushChange `json:"changes"`
}

type PushResult struct {
	ChangeID        string    `json:"change_id"`
	RecordID        string    `json:"record_id"`
	Status          string    `json:"status" example:"applied"`
	Version         int64     `json:"version,omitempty"`
	ServerUpdatedAt time.Time `json:"server_updated_at,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type PushResponse struct {
	Results []PushResult `json:"results"`
	Cursor  int64        `json:"cursor"`
}

type PullRecord struct {
	Collection      string          `json:"collection"`
	RecordID        string          `json:"record_id"`
	Deleted         bool            `json:"deleted"`
	Payload         json.RawMessage `json:"payload"`
	Version         int64           `json:"version"`
	ServerUpdatedAt time.Time       `json:"server_updated_at"`
}

type PullResponse struct {
	Changes []PullRecord `json:"changes"`
	Cursor  int64        `json:"cursor"`
	HasMore bool         `json:"has_more"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// handlers holds dependencies for the sync route handlers.
type handlers struct {
	store *syncdb.Store
}

// registerRoutes wires the sync routes. Both require auth — Core injects the
// X-ApiCoreX-* headers; handlers read user + schema from them.
func registerRoutes(p *plugin.Plugin, h *handlers) {
	p.POST("/sync/push", h.push,
		option.Summary("Push local changes"),
		option.Description("Applies client changes with last-write-wins; returns per-change status and a cursor."),
		option.Tags("sync"),
		option.Request(new(PushRequest)),
		option.Response(http.StatusOK, new(PushResponse)),
		option.Response(http.StatusUnauthorized, new(ErrorResponse)),
		option.Response(http.StatusBadRequest, new(ErrorResponse)),
	)
	p.GET("/sync/pull", h.pull,
		option.Summary("Pull server changes since cursor"),
		option.Description("Returns records (incl. tombstones) with version > since, ordered, paginated."),
		option.Tags("sync"),
		option.Response(http.StatusOK, new(PullResponse)),
		option.Response(http.StatusUnauthorized, new(ErrorResponse)),
	)
}

func (h *handlers) push(c *gin.Context) {
	userID, schema, ok := scope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "missing tenant/user context"})
		return
	}
	var req PushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
		return
	}
	if len(req.Changes) == 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "no changes in request"})
		return
	}
	if len(req.Changes) > maxPushBatch {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "too many changes in one push (max 1000)"})
		return
	}

	changes := make([]syncdb.Change, len(req.Changes))
	for i, ch := range req.Changes {
		changes[i] = syncdb.Change{
			ChangeID:   ch.ChangeID,
			Collection: ch.Collection,
			RecordID:   ch.RecordID,
			Deleted:    ch.Deleted,
			Payload:    ch.Payload,
			UpdatedAt:  ch.UpdatedAt,
		}
	}

	results, cursor, err := h.store.Push(c.Request.Context(), schema, userID, changes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	out := make([]PushResult, len(results))
	for i, r := range results {
		out[i] = PushResult{
			ChangeID: r.ChangeID, RecordID: r.RecordID, Status: r.Status,
			Version: r.Version, ServerUpdatedAt: r.ServerUpdatedAt, Error: r.Error,
		}
	}
	c.JSON(http.StatusOK, PushResponse{Results: out, Cursor: cursor})
}

func (h *handlers) pull(c *gin.Context) {
	userID, schema, ok := scope(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "missing tenant/user context"})
		return
	}
	var since int64
	if q := c.Query("since"); q != "" {
		v, err := strconv.ParseInt(q, 10, 64)
		if err != nil || v < 0 {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid 'since' cursor"})
			return
		}
		since = v
	}
	collection := c.Query("collection")
	limit := defaultPullLimit
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > maxPullLimit {
		limit = maxPullLimit
	}

	records, cursor, hasMore, err := h.store.Pull(c.Request.Context(), schema, userID, collection, since, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	out := make([]PullRecord, len(records))
	for i, r := range records {
		out[i] = PullRecord{
			Collection: r.Collection, RecordID: r.RecordID, Deleted: r.Deleted,
			Payload: r.Payload, Version: r.Version, ServerUpdatedAt: r.ServerUpdatedAt,
		}
	}
	c.JSON(http.StatusOK, PullResponse{Changes: out, Cursor: cursor, HasMore: hasMore})
}

// scope reads the trusted user + schema Core injected. Both must be present.
func scope(c *gin.Context) (userID, schema string, ok bool) {
	userID = plugin.UserID(c)
	schema = plugin.SchemaName(c)
	return userID, schema, userID != "" && schema != ""
}
