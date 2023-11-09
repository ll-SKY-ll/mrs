package services

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v4"

	"gitlab.com/etke.cc/mrs/api/model"
	"gitlab.com/etke.cc/mrs/api/utils"
)

const (
	// MaxCacheAge to be used on immutable resources
	MaxCacheAge   = "31536000"
	bunnyIPv4List = "https://bunnycdn.com/api/system/edgeserverlist"
	bunnyIPv6List = "https://bunnycdn.com/api/system/edgeserverlist/IPv6"
)

type cacheStats interface {
	Get() *model.IndexStats
}

var noncacheablePaths = map[string]struct{}{
	"/search":                            {},
	"/_matrix/federation/v1/publicRooms": {},
}

// Cache service
type Cache struct {
	cfg      ConfigService
	bunnyIPs map[string]struct{}
	stats    cacheStats
}

// NewCache service
func NewCache(cfg ConfigService, stats cacheStats) *Cache {
	cache := &Cache{
		cfg:      cfg,
		bunnyIPs: make(map[string]struct{}),
		stats:    stats,
	}
	cache.initBunnyIPs()
	return cache
}

func (cache *Cache) pullBunnyIPs(uri string) []string {
	resp, err := utils.Get(uri)
	if err != nil {
		utils.Logger.Error().Err(err).Msg("cannot get bunny ips")
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		utils.Logger.Error().Int("status_code", resp.StatusCode).Msg("cannot get bunny ips")
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.Logger.Error().Err(err).Msg("cannot read bunny ips")
		return nil
	}
	var ips []string
	if err := json.Unmarshal(body, &ips); err != nil {
		utils.Logger.Error().Err(err).Msg("cannot unmarshal bunny ips")
		return nil
	}
	return ips
}

func (cache *Cache) initBunnyIPs() {
	if cache.cfg.Get().Cache.Bunny.Key == "" {
		return
	}
	for _, ip := range append(cache.pullBunnyIPs(bunnyIPv4List), cache.pullBunnyIPs(bunnyIPv6List)...) {
		cache.bunnyIPs[ip] = struct{}{}
	}
	utils.Logger.Info().Int("count", len(cache.bunnyIPs)).Msg("bunny ips loaded")
}

// IsBunny returns true if the IP is a BunnyCDN IP
func (cache *Cache) IsBunny(ip string) bool {
	_, ok := cache.bunnyIPs[ip]
	return ok
}

// Middleware returns echo middleware
func (cache *Cache) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method != http.MethodGet {
				return next(c)
			}

			resp := c.Response()
			_, noncacheable := noncacheablePaths[c.Request().URL.Path]
			if noncacheable {
				resp.Header().Set("Cache-Control", "no-cache")
				return next(c)
			}

			lastModified := cache.stats.Get().Indexing.FinishedAt.Format(http.TimeFormat)
			ifModifiedSince := c.Request().Header.Get("if-modified-since")
			if lastModified == ifModifiedSince {
				return c.NoContent(http.StatusNotModified)
			}

			maxAge := strconv.Itoa(cache.cfg.Get().Cache.MaxAge)
			resp.Header().Set("Cache-Control", "max-age="+maxAge+", public")
			resp.Header().Set("CDN-Tag", "mutable")
			resp.Header().Set("Last-Modified", lastModified)
			return next(c)
		}
	}
}

// MiddlewareImmutable returns echo middleware with immutable in cache-control
func (cache *Cache) MiddlewareImmutable() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method != http.MethodGet {
				return next(c)
			}

			if c.Request().Header.Get("if-modified-since") != "" {
				return c.NoContent(http.StatusNotModified)
			}

			resp := c.Response()
			resp.Header().Del("Last-Modified")
			resp.Header().Set("CDN-Tag", "immutable")
			resp.Header().Set("Cache-Control", "max-age="+MaxCacheAge+", immutable")
			return next(c)
		}
	}
}

// Purge cache. At this moment works with BunnyCDN only
func (cache *Cache) Purge() {
	cache.purgeBunnyCDN()
}

// purgeBunnyCDN cache
// ref: https://docs.bunny.net/reference/pullzonepublic_purgecachepostbytag
func (cache *Cache) purgeBunnyCDN() {
	bunny := cache.cfg.Get().Cache.Bunny
	if bunny.Key == "" || bunny.URL == "" {
		return
	}
	req, err := http.NewRequest("POST", bunny.URL, bytes.NewBuffer([]byte(`{"CacheTag":"mutable"}}`)))
	if err != nil {
		utils.Logger.Error().Err(err).Msg("cannot purge bunny cache")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AccessKey", bunny.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		utils.Logger.Error().Err(err).Msg("cannot purge bunny cache")
		return
	}
	resp.Body.Close() // no need
	if resp.StatusCode != http.StatusNoContent {
		utils.Logger.Error().Err(err).Int("status_code", resp.StatusCode).Msg("cannot purge bunny cache")
	}
}
