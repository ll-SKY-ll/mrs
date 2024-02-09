package services

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"gitlab.com/etke.cc/mrs/api/metrics"
	"gitlab.com/etke.cc/mrs/api/model"
	"gitlab.com/etke.cc/mrs/api/utils"
)

type StatsRepository interface {
	DataRepository
	GetIndexStatsTL(prefix string) (map[time.Time]*model.IndexStats, error)
	SetIndexStatsTL(calculatedAt time.Time, stats *model.IndexStats) error
	GetIndexStats() *model.IndexStats
	SetIndexOnlineServers(servers int) error
	SetIndexIndexableServers(servers int) error
	SetIndexBlockedServers(servers int) error
	SetIndexParsedRooms(rooms int) error
	SetIndexIndexedRooms(rooms int) error
	SetIndexBannedRooms(rooms int) error
	SetIndexReportedRooms(rooms int) error
	SetStartedAt(process string, startedAt time.Time) error
	SetFinishedAt(process string, finishedAt time.Time) error
}

type Lenable interface {
	Len() int
}

// Stats service
type Stats struct {
	cfg        ConfigService
	data       StatsRepository
	block      Lenable
	index      Lenable
	stats      *model.IndexStats
	collecting bool
}

// NewStats service
func NewStats(cfg ConfigService, data StatsRepository, index, blocklist Lenable) *Stats {
	stats := &Stats{cfg: cfg, data: data, index: index, block: blocklist}
	stats.reload()

	return stats
}

// setMetrics updates /metrics endpoint with actual stats
func (s *Stats) setMetrics() {
	metrics.ServersOnline.Set(uint64(s.stats.Servers.Online))
	metrics.ServersIndexable.Set(uint64(s.stats.Servers.Indexable))
	metrics.RoomsParsed.Set(uint64(s.stats.Rooms.Parsed))
	metrics.RoomsIndexed.Set(uint64(s.stats.Rooms.Indexed))
}

// reload saved stats. Useful when you need to get updated timestamps, but don't want to parse whole db
func (s *Stats) reload() {
	s.stats = s.data.GetIndexStats()
	s.setMetrics()
}

// Get stats
func (s *Stats) Get() *model.IndexStats {
	return s.stats
}

// GetTL stats timeline
func (s *Stats) GetTL() map[time.Time]*model.IndexStats {
	tl, err := s.data.GetIndexStatsTL("")
	if err != nil {
		utils.Logger.Error().Err(err).Msg("cannot get stats timeline")
	}
	return tl
}

// SetStartedAt of the process
func (s *Stats) SetStartedAt(process string, startedAt time.Time) {
	if err := s.data.SetStartedAt(process, startedAt); err != nil {
		utils.Logger.Error().Err(err).Str("process", process).Msg("cannot set started_at")
	}
	s.stats = s.data.GetIndexStats()
}

// SetFinishedAt of the process
func (s *Stats) SetFinishedAt(process string, finishedAt time.Time) {
	if err := s.data.SetFinishedAt(process, finishedAt); err != nil {
		utils.Logger.Error().Err(err).Str("process", process).Msg("cannot set finished_at")
	}
	s.stats = s.data.GetIndexStats()
}

// CollectServers stats only
func (s *Stats) CollectServers(reload bool) {
	var online, indexable int
	s.data.FilterServers(func(server *model.MatrixServer) bool {
		if server.Online {
			online++
		}
		if server.Indexable {
			indexable++
		}
		return false
	})

	if err := s.data.SetIndexOnlineServers(online); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set online servers count")
	}

	if err := s.data.SetIndexIndexableServers(indexable); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set indexable servers count")
	}

	if err := s.data.SetIndexBlockedServers(s.block.Len()); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set blocked servers count")
	}

	if reload {
		s.reload()
	}
}

// Collect all stats from repository
func (s *Stats) Collect() {
	if s.collecting {
		utils.Logger.Info().Msg("stats collection already in progress, ignoring request")
		return
	}
	s.collecting = true
	defer func() { s.collecting = false }()

	s.CollectServers(false)

	var rooms int
	s.data.EachRoom(func(_ string, _ *model.MatrixRoom) bool {
		rooms++
		return false
	})
	if err := s.data.SetIndexParsedRooms(rooms); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set parsed rooms count")
	}
	if err := s.data.SetIndexIndexedRooms(s.index.Len()); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set indexed rooms count")
	}
	banned, berr := s.data.GetBannedRooms()
	if berr != nil {
		utils.Logger.Error().Err(berr).Msg("cannot get banned rooms count")
	}
	if err := s.data.SetIndexBannedRooms(len(banned)); err != nil {
		utils.Logger.Error().Err(berr).Msg("cannot set banned rooms count")
	}
	reported, rerr := s.data.GetReportedRooms()
	if rerr != nil {
		utils.Logger.Error().Err(berr).Msg("cannot get reported rooms count")
	}
	if err := s.data.SetIndexReportedRooms(len(reported)); err != nil {
		utils.Logger.Error().Err(berr).Msg("cannot set reported rooms count")
	}

	s.reload()
	if err := s.data.SetIndexStatsTL(time.Now().UTC(), s.stats); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot set stats timeline")
	}
	s.sendWebhook()
}

// sendWebhook send request to webhook if provided
func (s *Stats) sendWebhook() {
	if s.cfg.Get().Webhooks.Stats == "" {
		return
	}
	var user string
	parsedUIURL, err := url.Parse(s.cfg.Get().Public.UI)
	if err == nil {
		user = parsedUIURL.Hostname()
	}

	payload, err := json.Marshal(webhookPayload{
		Username: user,
		Markdown: s.getWebhookText(),
	})
	if err != nil {
		utils.Logger.Error().Err(err).Msg("webhook payload marshaling failed")
		return
	}

	req, err := http.NewRequest("POST", s.cfg.Get().Webhooks.Stats, bytes.NewReader(payload))
	if err != nil {
		utils.Logger.Error().Err(err).Msg("webhook request failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		utils.Logger.Error().Err(err).Msg("webhook sending failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		utils.Logger.Error().Err(err).Int("status_code", resp.StatusCode).Str("body", string(body)).Msg("webhook sending failed")
	}
}

func (s *Stats) getWebhookText() string {
	var text strings.Builder
	text.WriteString("**stats have been collected**\n\n")

	text.WriteString(fmt.Sprintf("* `%d` servers online (`%d` blocked)\n", s.stats.Servers.Online, s.stats.Servers.Blocked))
	text.WriteString(fmt.Sprintf("* `%d` rooms (`%d` blocked, `%d` reported)\n", s.stats.Rooms.Indexed, s.stats.Rooms.Banned, s.stats.Rooms.Reported))
	text.WriteString("\n---\n\n")

	discovery := s.stats.Discovery.FinishedAt.Sub(s.stats.Discovery.StartedAt)
	parsing := s.stats.Parsing.FinishedAt.Sub(s.stats.Parsing.StartedAt)
	indexing := s.stats.Indexing.FinishedAt.Sub(s.stats.Indexing.StartedAt)
	total := discovery + parsing + indexing

	text.WriteString(fmt.Sprintf("* `%s` took discovery process\n", discovery.String()))
	text.WriteString(fmt.Sprintf("* `%s` took parsing process\n", parsing.String()))
	text.WriteString(fmt.Sprintf("* `%s` took indexing process\n", indexing.String()))
	text.WriteString(fmt.Sprintf("* `%s` total\n", total.String()))

	return text.String()
}
