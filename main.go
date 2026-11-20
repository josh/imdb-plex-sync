package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/parquet-go/parquet-go"
)

const (
	indexURL           = "https://josh.github.io/plex-index/plex.parquet"
	defaultDiscoverURL = "https://discover.provider.plex.tv"
	maxAttempts        = 4
	watchlistPageSize  = 50
)

var retryStatuses = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusTooEarly:            true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

var verbose bool

func debugf(format string, v ...any) {
	if verbose {
		log.Printf(format, v...)
	}
}

type statusError struct {
	method string
	url    string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.method, e.url, e.status)
}

func fetch(req *http.Request, timeout time.Duration) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := fetchOnce(req, timeout)
		if err == nil {
			return body, nil
		}
		var se *statusError
		if (errors.As(err, &se) && !retryStatuses[se.status]) || attempt == maxAttempts-1 || req.Context().Err() != nil {
			return nil, err
		}
		delay := time.Second << attempt
		log.Printf("Retrying in %s after error: %v", delay, err)
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
}

func fetchOnce(req *http.Request, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.Clone(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{method: req.Method, url: req.URL.Redacted(), status: resp.StatusCode}
	}
	return body, nil
}

func readSource(ctx context.Context, source string) ([]byte, error) {
	u, err := url.Parse(source)
	if err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) {
		debugf("Fetching remote '%s'", source)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		return fetch(req, 10*time.Second)
	}
	debugf("Reading local file '%s'", source)
	return os.ReadFile(source)
}

func fetchIMDbWatchlist(ctx context.Context, source string) ([]string, error) {
	data, err := readSource(ctx, source)
	if err != nil {
		return nil, err
	}
	records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse IMDb watchlist: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("IMDb watchlist is empty")
	}
	col := slices.Index(records[0], "Const")
	if col < 0 {
		return nil, errors.New("IMDb watchlist has no Const column")
	}
	ids := make([]string, 0, len(records)-1)
	for _, record := range records[1:] {
		ids = append(ids, record[col])
	}
	return ids, nil
}

type indexRow struct {
	Key           []byte  `parquet:"key,optional"`
	IMDbNumericID *uint32 `parquet:"imdb_numeric_id,optional"`
}

func plexRatingKeys(ctx context.Context, imdbIDs []string) ([]string, error) {
	numericIDs := make([]uint32, 0, len(imdbIDs))
	wanted := make(map[uint32][]string, len(imdbIDs))
	for _, id := range imdbIDs {
		n, err := strconv.ParseUint(strings.TrimPrefix(id, "tt"), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid IMDb ID %q", id)
		}
		numericIDs = append(numericIDs, uint32(n))
		wanted[uint32(n)] = nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, err
	}
	data, err := fetch(req, 30*time.Second)
	if err != nil {
		return nil, err
	}
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open Plex index: %w", err)
	}
	reader := parquet.NewGenericReader[indexRow](file)
	defer reader.Close()
	rows := make([]indexRow, 1024)
	for {
		n, err := reader.Read(rows)
		for _, row := range rows[:n] {
			if row.Key == nil || row.IMDbNumericID == nil {
				continue
			}
			if keys, ok := wanted[*row.IMDbNumericID]; ok {
				wanted[*row.IMDbNumericID] = append(keys, hex.EncodeToString(row.Key))
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Plex index: %w", err)
		}
	}

	var ratingKeys []string
	mapped := 0
	for _, id := range numericIDs {
		if keys := wanted[id]; len(keys) > 0 {
			ratingKeys = append(ratingKeys, keys...)
			mapped++
		}
	}
	if mapped < len(imdbIDs) {
		log.Printf("Found %d/%d IMDb IDs", mapped, len(imdbIDs))
	} else {
		log.Printf("Found all %d IMDb IDs", len(imdbIDs))
	}
	return ratingKeys, nil
}

type discover struct {
	baseURL string
	token   string
}

func (d discover) do(ctx context.Context, method, path string, query url.Values, header http.Header) ([]byte, error) {
	u := strings.TrimRight(d.baseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Provider-Version", "7.2.0")
	if d.token != "" {
		req.Header.Set("X-Plex-Token", d.token)
	}
	return fetch(req, 30*time.Second)
}

func (d discover) watchlist(ctx context.Context) ([]string, error) {
	var keys []string
	for offset := 0; ; offset += watchlistPageSize {
		header := http.Header{}
		header.Set("X-Plex-Container-Start", strconv.Itoa(offset))
		header.Set("X-Plex-Container-Size", strconv.Itoa(watchlistPageSize))
		body, err := d.do(ctx, http.MethodGet, "/library/sections/watchlist/all", nil, header)
		if err != nil {
			return nil, err
		}
		var page struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey string `json:"ratingKey"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("parse Plex watchlist: %w", err)
		}
		for _, m := range page.MediaContainer.Metadata {
			if m.RatingKey != "" {
				keys = append(keys, m.RatingKey)
			}
		}
		if len(page.MediaContainer.Metadata) < watchlistPageSize {
			return keys, nil
		}
	}
}

func (d discover) action(ctx context.Context, action, key string) error {
	body, err := d.do(ctx, http.MethodPut, "/actions/"+action, url.Values{"ratingKey": {key}}, nil)
	if err != nil {
		return err
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("%s %s: %w", action, key, err)
	}
	if len(v) == 0 {
		return fmt.Errorf("%s %s: empty response", action, key)
	}
	return nil
}

func envBool(name string) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %q", name, v)
	}
	return b, nil
}

func run(ctx context.Context) error {
	dryRun, err := envBool("DRY_RUN")
	if err != nil {
		return err
	}
	discoverURL := os.Getenv("PLEX_DISCOVER_URL")
	if discoverURL == "" {
		discoverURL = defaultDiscoverURL
	}

	watchlistURL := flag.String("imdb-watchlist-url", os.Getenv("IMDB_WATCHLIST_URL"), "IMDb watchlist CSV URL or path (env IMDB_WATCHLIST_URL)")
	flag.StringVar(&discoverURL, "plex-discover-url", discoverURL, "Plex Discover base URL (env PLEX_DISCOVER_URL)")
	flag.BoolVar(&dryRun, "dry-run", dryRun, "show what would be done without making changes (env DRY_RUN)")
	flag.BoolVar(&dryRun, "n", dryRun, "shorthand for -dry-run")
	flag.BoolVar(&verbose, "v", false, "enable verbose logging")
	flag.Parse()

	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flag.Args(), " "))
	}
	if *watchlistURL == "" {
		return errors.New("IMDB_WATCHLIST_URL or -imdb-watchlist-url is required")
	}
	plex := discover{baseURL: discoverURL, token: os.Getenv("PLEX_TOKEN")}
	if plex.token == "" && discoverURL == defaultDiscoverURL {
		return errors.New("PLEX_TOKEN is required unless PLEX_DISCOVER_URL points at an authenticating proxy")
	}

	imdbIDs, err := fetchIMDbWatchlist(ctx, *watchlistURL)
	if err != nil {
		return err
	}
	ratingKeys, err := plexRatingKeys(ctx, imdbIDs)
	if err != nil {
		return err
	}
	watchlist, err := plex.watchlist(ctx)
	if err != nil {
		return err
	}

	imdbKeys := make(map[string]bool, len(ratingKeys))
	for _, k := range ratingKeys {
		imdbKeys[k] = true
	}
	plexKeys := make(map[string]bool, len(watchlist))
	for _, k := range watchlist {
		plexKeys[k] = true
	}

	failures := 0
	for _, key := range slices.Sorted(maps.Keys(imdbKeys)) {
		if plexKeys[key] {
			continue
		}
		if dryRun {
			log.Printf("[DRY RUN] + %s", key)
			continue
		}
		log.Printf("+ %s", key)
		if err := plex.action(ctx, "addToWatchlist", key); err != nil {
			log.Printf("Failed to add %s: %v", key, err)
			failures++
		}
	}
	for _, key := range slices.Sorted(maps.Keys(plexKeys)) {
		if imdbKeys[key] {
			continue
		}
		if dryRun {
			log.Printf("[DRY RUN] - %s", key)
			continue
		}
		log.Printf("- %s", key)
		if err := plex.action(ctx, "removeFromWatchlist", key); err != nil {
			log.Printf("Failed to remove %s: %v", key, err)
			failures++
		}
	}

	if failures > 0 {
		return fmt.Errorf("%d watchlist changes failed", failures)
	}
	return nil
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
