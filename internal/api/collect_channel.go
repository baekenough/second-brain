package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/baekenough/second-brain/internal/scheduler"
)

// collectSlackChannelRequest is the JSON body for POST /api/v1/collect/slack/channel.
// Either channel_id or channel_name is required.
type collectSlackChannelRequest struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

// collectSlackChannelResponse is the JSON body returned on success.
type collectSlackChannelResponse struct {
	Status      string `json:"status"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Upserted    int    `json:"upserted"`
}

// collectSlackChannelHandler handles POST /api/v1/collect/slack/channel.
// It triggers a full-history (since=zero) collection for a single Slack channel
// and blocks until the collection completes. The caller must supply at least one
// of channel_id or channel_name in the JSON body.
func (s *Server) collectSlackChannelHandler(w http.ResponseWriter, r *http.Request) {
	var req collectSlackChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.ChannelID == "" && req.ChannelName == "" {
		writeError(w, http.StatusBadRequest, "channel_id or channel_name is required")
		return
	}

	// When only channel_name is provided, resolve the channel ID first so that
	// we can return a meaningful 404 before starting the collection.
	channelID := req.ChannelID
	channelName := req.ChannelName

	if channelID == "" {
		id, name, err := s.scheduler.LookupSlackChannel(r.Context(), channelName)
		if err != nil {
			if errors.Is(err, scheduler.ErrSlackCollectorNotFound) {
				writeError(w, http.StatusInternalServerError, "slack collector not configured")
				return
			}
			if errors.Is(err, scheduler.ErrSlackChannelNotFound) {
				writeError(w, http.StatusNotFound, "channel not found in bot member list")
				return
			}
			writeError(w, http.StatusInternalServerError, "channel lookup failed")
			return
		}
		channelID = id
		channelName = name
	}

	upserted, err := s.scheduler.ForceCollectSlackChannel(r.Context(), channelID, channelName)
	if err != nil {
		if errors.Is(err, scheduler.ErrSlackCollectorNotFound) {
			writeError(w, http.StatusInternalServerError, "slack collector not configured")
			return
		}
		if errors.Is(err, scheduler.ErrSlackChannelNotFound) {
			writeError(w, http.StatusNotFound, "channel not found in bot member list")
			return
		}
		writeError(w, http.StatusInternalServerError, "collection failed")
		return
	}

	writeJSON(w, http.StatusOK, collectSlackChannelResponse{
		Status:      "collected",
		ChannelID:   channelID,
		ChannelName: channelName,
		Upserted:    upserted,
	})
}
