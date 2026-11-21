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
	"log/slog"
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
	Version            = "0.1.2"
	defaultDiscoverURL = "https://discover.provider.plex.tv"
	maxAttempts        = 4
	watchlistPageSize  = 50
	metadataBatchSize  = 100
	maxMatchLookups    = 100
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

type statusError struct {
	method string
	url    string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.method, e.url, e.status)
}

func since(start time.Time) time.Duration {
	return time.Since(start).Round(time.Millisecond)
}

func fetch(req *http.Request, timeout time.Duration) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		start := time.Now()
		body, status, err := fetchOnce(req, timeout)
		slog.Debug("http request",
			"method", req.Method,
			"url", req.URL.Redacted(),
			"attempt", attempt,
			"status", status,
			"bytes", len(body),
			"duration", since(start),
		)
		if err == nil {
			return body, nil
		}
		var se *statusError
		if (errors.As(err, &se) && !retryStatuses[se.status]) || attempt == maxAttempts || req.Context().Err() != nil {
			return nil, err
		}
		delay := time.Second << (attempt - 1)
		slog.Warn("retrying http request",
			"method", req.Method,
			"url", req.URL.Redacted(),
			"attempt", attempt,
			"delay", delay,
			"err", err,
		)
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
}

func fetchOnce(req *http.Request, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.Clone(ctx))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body, resp.StatusCode, &statusError{method: req.Method, url: req.URL.Redacted(), status: resp.StatusCode}
	}
	return body, resp.StatusCode, nil
}

func readSource(ctx context.Context, source string, timeout time.Duration) ([]byte, error) {
	u, err := url.Parse(source)
	if err == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		return fetch(req, timeout)
	}
	slog.Debug("reading local file", "path", source)
	return os.ReadFile(source)
}

type imdbTitle struct {
	ID    string
	Title string
	Show  bool
}

func fetchIMDbWatchlist(ctx context.Context, source string) ([]imdbTitle, error) {
	start := time.Now()
	data, err := readSource(ctx, source, 10*time.Second)
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
	idCol := slices.Index(records[0], "Const")
	if idCol < 0 {
		return nil, errors.New("IMDb watchlist has no Const column")
	}
	titleCol := slices.Index(records[0], "Title")
	typeCol := slices.Index(records[0], "Title Type")
	titles := make([]imdbTitle, 0, len(records)-1)
	for _, record := range records[1:] {
		t := imdbTitle{ID: record[idCol]}
		if titleCol >= 0 {
			t.Title = record[titleCol]
		}
		if typeCol >= 0 {
			switch record[typeCol] {
			case "TV Series", "TV Mini Series":
				t.Show = true
			}
		}
		titles = append(titles, t)
	}
	slog.Info("fetched IMDb watchlist", "titles", len(titles), "bytes", len(data), "duration", since(start))
	return titles, nil
}

type indexRow struct {
	Key           []byte  `parquet:"key,optional"`
	IMDbNumericID *uint32 `parquet:"imdb_numeric_id,optional"`
}

func plexRatingKeys(ctx context.Context, source string, imdbIDs []string) (map[string][]string, error) {
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

	start := time.Now()
	data, err := readSource(ctx, source, 30*time.Second)
	if err != nil {
		return nil, err
	}
	slog.Info("fetched Plex index", "bytes", len(data), "duration", since(start))

	start = time.Now()
	file, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open Plex index: %w", err)
	}
	reader := parquet.NewGenericReader[indexRow](file)
	defer reader.Close()
	scanned := 0
	rows := make([]indexRow, 1024)
	for {
		n, err := reader.Read(rows)
		scanned += n
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
	slog.Info("scanned Plex index", "rows", scanned, "row_groups", len(file.RowGroups()), "duration", since(start))

	ratingKeys := make(map[string][]string)
	var unmapped []string
	for i, id := range numericIDs {
		keys := wanted[id]
		if len(keys) == 0 {
			unmapped = append(unmapped, imdbIDs[i])
			continue
		}
		slog.Debug("mapped IMDb ID", "imdb_id", imdbIDs[i], "rating_keys", keys)
		ratingKeys[imdbIDs[i]] = keys
	}
	mapped := len(imdbIDs) - len(unmapped)
	slog.Info("mapped IMDb watchlist to Plex", "mapped", mapped, "total", len(imdbIDs))
	if len(unmapped) > 0 {
		slog.Warn("IMDb IDs missing from Plex index", "count", len(unmapped), "imdb_ids", unmapped)
	}
	return ratingKeys, nil
}

type discover struct {
	baseURL string
	token   string
}

type watchlistItem struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Type      string `json:"type"`
	PlexGUID  string `json:"guid"`
	Guid      []struct {
		ID string `json:"id"`
	} `json:"Guid"`
}

func (item watchlistItem) imdbID() string {
	for _, guid := range item.Guid {
		if id, ok := strings.CutPrefix(guid.ID, "imdb://"); ok {
			return id
		}
	}
	return ""
}

type metadataResponse struct {
	MediaContainer struct {
		Metadata []watchlistItem `json:"Metadata"`
	} `json:"MediaContainer"`
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

func (d discover) watchlist(ctx context.Context) (map[string]watchlistItem, error) {
	start := time.Now()
	items := make(map[string]watchlistItem)
	for page, offset := 1, 0; ; page, offset = page+1, offset+watchlistPageSize {
		header := http.Header{}
		header.Set("X-Plex-Container-Start", strconv.Itoa(offset))
		header.Set("X-Plex-Container-Size", strconv.Itoa(watchlistPageSize))
		body, err := d.do(ctx, http.MethodGet, "/library/sections/watchlist/all", url.Values{"includeGuids": {"1"}}, header)
		if err != nil {
			return nil, err
		}
		var resp metadataResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parse Plex watchlist: %w", err)
		}
		for _, item := range resp.MediaContainer.Metadata {
			if item.RatingKey == "" {
				slog.Debug("skipping Plex watchlist item without rating key", "title", item.Title, "year", item.Year)
				continue
			}
			slog.Debug("Plex watchlist item", "rating_key", item.RatingKey, "imdb_id", item.imdbID(), "title", item.Title, "year", item.Year, "type", item.Type)
			items[item.RatingKey] = item
		}
		if len(resp.MediaContainer.Metadata) < watchlistPageSize {
			slog.Info("fetched Plex watchlist", "items", len(items), "pages", page, "duration", since(start))
			return items, nil
		}
	}
}

func (d discover) metadata(ctx context.Context, keys []string) (map[string]watchlistItem, error) {
	start := time.Now()
	items := make(map[string]watchlistItem, len(keys))
	for batch := range slices.Chunk(keys, metadataBatchSize) {
		escaped := make([]string, 0, len(batch))
		for _, key := range batch {
			escaped = append(escaped, url.PathEscape(key))
		}
		header := http.Header{}
		header.Set("X-Plex-Container-Start", "0")
		header.Set("X-Plex-Container-Size", strconv.Itoa(metadataBatchSize))
		body, err := d.do(ctx, http.MethodGet, "/library/metadata/"+strings.Join(escaped, ","), url.Values{"includeGuids": {"1"}}, header)
		if err != nil {
			return nil, err
		}
		var resp metadataResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parse Plex metadata: %w", err)
		}
		for _, item := range resp.MediaContainer.Metadata {
			items[item.RatingKey] = item
		}
	}
	slog.Info("checked Plex index rating keys", "keys", len(keys), "found", len(items), "duration", since(start))
	return items, nil
}

func (d discover) match(ctx context.Context, t imdbTitle) (watchlistItem, bool, error) {
	plexType := "1"
	if t.Show {
		plexType = "2"
	}
	body, err := d.do(ctx, http.MethodGet, "/library/metadata/matches", url.Values{"type": {plexType}, "guid": {"imdb://" + t.ID}}, nil)
	if err != nil {
		return watchlistItem{}, false, err
	}
	var resp metadataResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return watchlistItem{}, false, fmt.Errorf("parse Plex match %s: %w", t.ID, err)
	}
	for _, item := range resp.MediaContainer.Metadata {
		if item.RatingKey != "" && item.imdbID() == t.ID {
			return item, true, nil
		}
	}
	return watchlistItem{}, false, nil
}

func verifiedKey(t imdbTitle, candidates []string, found map[string]watchlistItem) string {
	for _, key := range candidates {
		item, ok := found[key]
		switch {
		case !ok:
			slog.Warn("Plex index rating key not found in Plex Discover", "imdb_id", t.ID, "title", t.Title, "rating_key", key)
		case item.imdbID() != t.ID:
			slog.Warn("Plex index rating key belongs to another title",
				"imdb_id", t.ID,
				"title", t.Title,
				"rating_key", key,
				"plex_imdb_id", item.imdbID(),
				"plex_title", item.Title,
			)
		default:
			return key
		}
	}
	return ""
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

func run(ctx context.Context, level *slog.LevelVar) error {
	start := time.Now()
	dryRun, err := envBool("DRY_RUN")
	if err != nil {
		return err
	}
	discoverURL := os.Getenv("PLEX_DISCOVER_URL")
	if discoverURL == "" {
		discoverURL = defaultDiscoverURL
	}

	watchlistURL := flag.String("imdb-watchlist-url", os.Getenv("IMDB_WATCHLIST_URL"), "IMDb watchlist CSV URL or path (env IMDB_WATCHLIST_URL)")
	indexSource := flag.String("plex-index-url", os.Getenv("PLEX_INDEX_URL"), "Plex index parquet URL or path (env PLEX_INDEX_URL)")
	flag.StringVar(&discoverURL, "plex-discover-url", discoverURL, "Plex Discover base URL (env PLEX_DISCOVER_URL)")
	flag.BoolVar(&dryRun, "dry-run", dryRun, "show what would be done without making changes (env DRY_RUN)")
	flag.BoolVar(&dryRun, "n", dryRun, "shorthand for -dry-run")
	verbose := flag.Bool("v", false, "enable verbose logging")
	showVersion := flag.Bool("version", false, "print version information")
	flag.BoolVar(showVersion, "V", false, "shorthand for -version")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return nil
	}
	if *verbose {
		level.Set(slog.LevelDebug)
	}
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flag.Args(), " "))
	}
	if *watchlistURL == "" {
		return errors.New("IMDB_WATCHLIST_URL or -imdb-watchlist-url is required")
	}
	if *indexSource == "" {
		return errors.New("PLEX_INDEX_URL or -plex-index-url is required")
	}
	plex := discover{baseURL: discoverURL, token: os.Getenv("PLEX_TOKEN")}
	if plex.token == "" && discoverURL == defaultDiscoverURL {
		return errors.New("PLEX_TOKEN is required unless PLEX_DISCOVER_URL points at an authenticating proxy")
	}

	slog.Info("starting sync",
		"version", Version,
		"imdb_watchlist", *watchlistURL,
		"plex_index", *indexSource,
		"plex_discover_url", discoverURL,
		"plex_token", plex.token != "",
		"dry_run", dryRun,
	)

	titles, err := fetchIMDbWatchlist(ctx, *watchlistURL)
	if err != nil {
		return err
	}
	imdbIDs := make([]string, 0, len(titles))
	for _, t := range titles {
		imdbIDs = append(imdbIDs, t.ID)
	}
	candidates, err := plexRatingKeys(ctx, *indexSource, imdbIDs)
	if err != nil {
		return err
	}
	plexItems, err := plex.watchlist(ctx)
	if err != nil {
		return err
	}

	onPlex := make(map[string]bool)
	for _, item := range plexItems {
		if id := item.imdbID(); id != "" {
			onPlex[id] = true
		}
	}

	type addition struct {
		key   string
		title imdbTitle
	}
	wantedIDs := make(map[string]bool)
	wantedKeys := make(map[string]bool)
	var pending []imdbTitle
	var pendingKeys []string
	unchanged, failures := 0, 0
	for _, t := range titles {
		if wantedIDs[t.ID] {
			continue
		}
		wantedIDs[t.ID] = true
		for _, key := range candidates[t.ID] {
			wantedKeys[key] = true
		}
		if onPlex[t.ID] || slices.ContainsFunc(candidates[t.ID], func(key string) bool { _, ok := plexItems[key]; return ok }) {
			unchanged++
			continue
		}
		pending = append(pending, t)
		pendingKeys = append(pendingKeys, candidates[t.ID]...)
	}

	found, err := plex.metadata(ctx, pendingKeys)
	if err != nil {
		return err
	}

	var toAdd []addition
	var unresolved, skipped []string
	lookups := 0
	for _, t := range pending {
		key := verifiedKey(t, candidates[t.ID], found)
		if key == "" {
			if lookups == maxMatchLookups {
				skipped = append(skipped, t.ID)
				continue
			}
			lookups++
			item, ok, err := plex.match(ctx, t)
			if err != nil {
				slog.Error("failed to look up IMDb title in Plex Discover", "imdb_id", t.ID, "title", t.Title, "err", err)
				failures++
				continue
			}
			if !ok {
				unresolved = append(unresolved, t.ID)
				continue
			}
			slog.Info("matched IMDb title in Plex Discover", "imdb_id", t.ID, "title", t.Title, "rating_key", item.RatingKey)
			key = item.RatingKey
		}
		wantedKeys[key] = true
		if _, ok := plexItems[key]; ok {
			unchanged++
			continue
		}
		toAdd = append(toAdd, addition{key: key, title: t})
	}
	if len(unresolved) > 0 {
		slog.Warn("IMDb titles not found in Plex Discover", "count", len(unresolved), "imdb_ids", unresolved)
	}
	if len(skipped) > 0 {
		slog.Warn("skipped Plex Discover lookups over the per-run limit", "limit", maxMatchLookups, "count", len(skipped), "imdb_ids", skipped)
	}

	var toRemove []string
	for _, key := range slices.Sorted(maps.Keys(plexItems)) {
		if !wantedKeys[key] && !wantedIDs[plexItems[key].imdbID()] {
			toRemove = append(toRemove, key)
		}
	}
	slog.Info("planned watchlist changes",
		"add", len(toAdd),
		"remove", len(toRemove),
		"unchanged", unchanged,
		"unresolved", len(unresolved),
		"skipped", len(skipped),
		"dry_run", dryRun,
	)

	added, removed := 0, 0
	for _, a := range toAdd {
		key := a.key
		attrs := []any{"rating_key", key, "imdb_id", a.title.ID, "title", a.title.Title}
		if dryRun {
			slog.Info("would add to Plex watchlist", attrs...)
			continue
		}
		if err := plex.action(ctx, "addToWatchlist", key); err != nil {
			slog.Error("failed to add to Plex watchlist", append(attrs, "err", err)...)
			failures++
			continue
		}
		slog.Info("added to Plex watchlist", attrs...)
		added++
	}
	for _, key := range toRemove {
		item := plexItems[key]
		attrs := []any{"rating_key", key, "title", item.Title, "year", item.Year, "type", item.Type}
		if dryRun {
			slog.Info("would remove from Plex watchlist", attrs...)
			continue
		}
		if err := plex.action(ctx, "removeFromWatchlist", key); err != nil {
			slog.Error("failed to remove from Plex watchlist", append(attrs, "err", err)...)
			failures++
			continue
		}
		slog.Info("removed from Plex watchlist", attrs...)
		removed++
	}

	slog.Info("finished sync",
		"added", added,
		"removed", removed,
		"failed", failures,
		"dry_run", dryRun,
		"duration", since(start),
	)
	if failures > 0 {
		return fmt.Errorf("%d watchlist changes failed", failures)
	}
	return nil
}

func main() {
	level := new(slog.LevelVar)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, level)
	stop()
	if err != nil {
		slog.Error("sync failed", "err", err)
		os.Exit(1)
	}
}
