package matrix

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/goccy/go-json"
	"github.com/rs/zerolog"

	"gitlab.com/etke.cc/mrs/api/model"
	"gitlab.com/etke.cc/mrs/api/utils"
)

// getErrorResp returns canonical json of matrix error
func (s *Server) getErrorResp(ctx context.Context, code, message string) []byte {
	respb, err := utils.JSON(model.MatrixError{
		Code:    code,
		Message: message,
	})
	if err != nil {
		zerolog.Ctx(ctx).Error().Err(err).Msg("cannot marshal canonical json")
	}
	return respb
}

func (s *Server) parseErrorResp(status string, body []byte) *model.MatrixError {
	if len(body) == 0 {
		return nil
	}
	var merr *model.MatrixError
	if err := json.Unmarshal(body, &merr); err != nil {
		return nil
	}
	if merr.Code == "" {
		return nil
	}

	merr.HTTP = status
	return merr
}

// parseClientWellKnown returns URL of the Matrix CS API server
func (s *Server) parseClientWellKnown(ctx context.Context, serverName string) (string, error) {
	span := sentry.StartSpan(ctx, "matrix.parseClientWellKnown")
	defer span.Finish()

	resp, err := utils.Get(ctx, "https://"+serverName+"/.well-known/matrix/client")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("no /.well-known/matrix/client")
	}

	datab, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var wellknown *wellKnownClientResp
	if wkerr := json.Unmarshal(datab, &wellknown); wkerr != nil {
		return "", wkerr
	}
	if wellknown.Homeserver.BaseURL == "" {
		return "", fmt.Errorf("/.well-known/matrix/client is empty")
	}
	return wellknown.Homeserver.BaseURL, nil
}

// parseServerWellKnown returns Federation API host:port
func (s *Server) parseServerWellKnown(ctx context.Context, serverName string) (string, error) {
	span := sentry.StartSpan(ctx, "matrix.parseServerWellKnown")
	defer span.Finish()

	resp, err := utils.Get(ctx, "https://"+serverName+"/.well-known/matrix/server")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("no /.well-known/matrix/server")
	}

	datab, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var wellknown *wellKnownServerResp
	if wkerr := json.Unmarshal(datab, &wellknown); wkerr != nil {
		return "", wkerr
	}
	if wellknown.Host == "" {
		return "", fmt.Errorf("/.well-known/matrix/server is empty")
	}
	parts := strings.Split(wellknown.Host, ":")
	if len(parts) == 0 {
		return "", fmt.Errorf("/.well-known/matrix/server is invalid")
	}
	host := parts[0]
	port := "8448"
	if len(parts) == 2 {
		port = parts[1]
	}
	return host + ":" + port, err
}

// parseSRV returns Federation API host:port
func (s *Server) parseSRV(ctx context.Context, service, serverName string) (string, error) {
	span := sentry.StartSpan(ctx, "matrix.parseSRV")
	defer span.Finish()

	_, addrs, err := net.LookupSRV(service, "tcp", serverName)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("no " + service + " SRV records")
	}
	return strings.Trim(addrs[0].Target, ".") + ":" + strconv.Itoa(int(addrs[0].Port)), nil
}

// dcrURL stands for discover-cache-and-return URL, shortcut for s.getURL
func (s *Server) dcrURL(ctx context.Context, serverName, url string, discover bool) string {
	s.surlsCache.Add(serverName, url)

	if s.discoverFunc != nil && discover {
		go s.discoverFunc(ctx, serverName)
	}

	return url
}

// getURL returns Federation API URL
func (s *Server) getURL(ctx context.Context, serverName string, discover bool) string {
	span := sentry.StartSpan(ctx, "matrix.getURL")
	defer span.Finish()

	cached, ok := s.surlsCache.Get(serverName)
	if ok {
		return cached
	}

	fromWellKnown, err := s.parseServerWellKnown(span.Context(), serverName)
	if err == nil {
		return s.dcrURL(span.Context(), serverName, "https://"+fromWellKnown, discover)
	}
	fromSRV, err := s.parseSRV(span.Context(), "matrix-fed", serverName)
	if err == nil {
		return s.dcrURL(span.Context(), serverName, "https://"+fromSRV, discover)
	}
	fromSRV, err = s.parseSRV(span.Context(), "matrix", serverName)
	if err == nil {
		return s.dcrURL(span.Context(), serverName, "https://"+fromSRV, discover)
	}

	return s.dcrURL(span.Context(), serverName, "https://"+serverName+":8448", discover)
}

// lookupKeys requests /_matrix/key/v2/server by serverName
func (s *Server) lookupKeys(ctx context.Context, serverName string, discover bool) (*matrixKeyResp, error) {
	span := sentry.StartSpan(ctx, "matrix.lookupKeys")
	defer span.Finish()

	keysURL, err := url.Parse(s.getURL(span.Context(), serverName, discover) + "/_matrix/key/v2/server")
	if err != nil {
		return nil, err
	}
	resp, err := utils.Get(span.Context(), keysURL.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	datab, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if merr := s.parseErrorResp(resp.Status, datab); merr != nil {
		return nil, merr
	}

	var keysResp *matrixKeyResp
	if err := json.Unmarshal(datab, &keysResp); err != nil {
		return nil, err
	}
	return keysResp, nil
}

// queryKeys returns serverName's keys
func (s *Server) queryKeys(ctx context.Context, serverName string) map[string]ed25519.PublicKey {
	cached, ok := s.keysCache.Get(serverName)
	if ok {
		return cached
	}
	log := zerolog.Ctx(ctx)
	resp, err := s.lookupKeys(ctx, serverName, true)
	if err != nil {
		log.Warn().Err(err).Msg("keys query failed")
		return nil
	}
	if resp.ServerName != serverName {
		log.Warn().Msg("server name doesn't match")
		return nil
	}
	if resp.ValidUntilTS <= time.Now().UnixMilli() {
		log.Warn().Msg("server keys are expired")
	}
	keys := map[string]ed25519.PublicKey{}
	for id, data := range resp.VerifyKeys {
		pub, err := base64.RawStdEncoding.DecodeString(data["key"])
		if err != nil {
			log.Warn().Err(err).Msg("failed to decode server key")
			continue
		}
		keys[id] = pub
	}
	// TODO: verify signatures
	s.keysCache.Add(serverName, keys)
	return keys
}
