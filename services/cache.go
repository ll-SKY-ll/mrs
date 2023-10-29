package services

import (
	"bytes"
	"log"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"gitlab.com/etke.cc/mrs/api/model"
)

// MaxCacheAge to be used on immutable resources
const MaxCacheAge = "31536000"

type cacheStats interface {
	Get() *model.IndexStats
}

var noncacheablePaths = map[string]struct{}{
	"/search":                            {},
	"/_matrix/federation/v1/publicRooms": {},
}

// Cache service
type Cache struct {
	cfg   ConfigService
	stats cacheStats
}

// NewCache service
func NewCache(cfg ConfigService, stats cacheStats) *Cache {
	return &Cache{
		cfg:   cfg,
		stats: stats,
	}
}

// Middleware returns echo middleware
func (cache *Cache) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method != http.MethodGet {
				return next(c)
			}

			_, noncacheable := noncacheablePaths[c.Request().URL.Path]
			lastModified := cache.stats.Get().Indexing.FinishedAt.Format(http.TimeFormat)
			ifModifiedSince := c.Request().Header.Get("if-modified-since")
			if lastModified == ifModifiedSince && !noncacheable {
				return c.NoContent(http.StatusNotModified)
			}

			resp := c.Response()
			maxAge := strconv.Itoa(cache.cfg.Get().Cache.MaxAge)
			resp.Header().Set("Cache-Control", "max-age="+maxAge+", public")
			resp.Header().Set("CDN-Tag", "mutable")
			if !noncacheable {
				resp.Header().Set("Last-Modified", lastModified)
			}
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
		log.Println("cannot purge bunny cache", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AccessKey", bunny.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println("cannot purge bunny cache", err)
		return
	}
	resp.Body.Close() // no need
	if resp.StatusCode != http.StatusNoContent {
		log.Println("cannot purge bunny cache - http status code is", resp.StatusCode)
	}
}
