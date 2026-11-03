// SPDX-License-Identifier: AGPL-3.0-or-later

// Package yt_downloader integrates Hister with the local downloader API.
package yt_downloader

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/extractor/sdk"
	medialifecycle "github.com/asciimoo/hister/server/media"
)

const (
	defaultBinary              = "yt_downloader"
	defaultOutputDir           = "~/Videos/Hister/YouTube"
	MediaReferencesMetadataKey = "yt_downloader_media"
	maxDiagnosticSize          = 64 << 10
	maxManifestSize            = 1 << 20
)

const manifestUpgradeHint = "requires yt_downloader named-job --result-json support; upgrade tools--yt_downloader"

// YTDownloader downloads video and audio files without replacing the
// document content extracted by Hister's normal content extractor chain.
type YTDownloader struct {
	cfg               *sdk.Config
	jobSlots          chan struct{}
	downloadsMu       sync.Mutex
	downloads         map[string]*downloadCall
	publicationMu     sync.Mutex
	nextPublicationID uint64
	publications      map[string]*publicationState
}

type publicationState struct {
	active              int
	acceptedPublication uint64
	acceptedReferences  string
}

type downloadCall struct {
	done   chan struct{}
	result *downloadResult
	err    error
}

var (
	_ sdk.Extractor        = (*YTDownloader)(nil)
	_ sdk.ContextExtractor = (*YTDownloader)(nil)
)

func (e *YTDownloader) Name() string { return "yt_downloader" }

func (e *YTDownloader) Description() string {
	return "Queues video and audio downloads; its preview shows trusted status and ready playback links."
}

func (e *YTDownloader) Capabilities() sdk.Capabilities {
	return sdk.Capabilities{Enrich: true, Preview: true}
}

func (e *YTDownloader) GetConfig() *sdk.Config {
	if e.cfg != nil {
		return e.cfg
	}
	return &sdk.Config{
		Enable: false,
		Options: map[string]any{
			"binary":              defaultBinary,
			"output_dir":          defaultOutputDir,
			"timeout":             1800,
			"max_concurrent_jobs": 2,
			"backend":             "auto",
			"transcribe":          false,
		},
	}
}

func (e *YTDownloader) SetConfig(cfg *sdk.Config) error {
	if cfg == nil {
		return errors.New("configuration is nil")
	}
	for key := range cfg.Options {
		switch key {
		case "binary", "output_dir", "timeout", "max_concurrent_jobs", "backend", "transcribe":
		default:
			return fmt.Errorf("unknown option %q", key)
		}
	}
	if value, ok := cfg.Options["binary"]; ok {
		if _, valid := value.(string); !valid || strings.TrimSpace(value.(string)) == "" {
			return errors.New("binary must be a non-empty executable path")
		}
	}
	if value, ok := cfg.Options["output_dir"]; ok {
		if _, valid := value.(string); !valid || strings.TrimSpace(value.(string)) == "" {
			return errors.New("output_dir must be a non-empty directory path")
		}
	}
	if value, ok := cfg.Options["backend"]; ok {
		backend, valid := value.(string)
		if !valid || (backend != "auto" && backend != "yt-dlp" && backend != "youtube-dl") {
			return errors.New("backend must be auto, yt-dlp, or youtube-dl")
		}
	}
	if value, ok := cfg.Options["timeout"]; ok {
		seconds, err := integerOption(value, "timeout")
		if err != nil || seconds < 1 || seconds > 7200 {
			return errors.New("timeout must be an integer from 1 to 7200 seconds")
		}
	}
	if value, ok := cfg.Options["transcribe"]; ok {
		if _, valid := value.(bool); !valid {
			return errors.New("transcribe must be a boolean")
		}
	}
	if value, ok := cfg.Options["max_concurrent_jobs"]; ok {
		jobs, err := integerOption(value, "max_concurrent_jobs")
		if err != nil || jobs < 0 {
			return errors.New("max_concurrent_jobs must be a non-negative integer")
		}
	}

	maxJobs := 2
	if value, ok := cfg.Options["max_concurrent_jobs"]; ok {
		jobs, _ := integerOption(value, "max_concurrent_jobs")
		maxJobs = jobs
	}
	if maxJobs == 0 {
		e.jobSlots = nil
	} else {
		e.jobSlots = make(chan struct{}, maxJobs)
	}
	e.cfg = cfg
	return nil
}

func integerOption(value any, key string) (int, error) {
	switch number := value.(type) {
	case int:
		return number, nil
	case int64:
		return int(number), nil
	case float64:
		integer := int(number)
		if float64(integer) == number {
			return integer, nil
		}
	}
	return 0, fmt.Errorf("%s must be an integer", key)
}

func (e *YTDownloader) Match(d *sdk.Document) bool {
	parsed, err := url.Parse(d.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "youtu.be" && host != "youtube-nocookie.com" && !strings.HasSuffix(host, ".youtube-nocookie.com") {
		return false
	}
	return strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != ""
}

func (e *YTDownloader) Extract(d *sdk.Document) sdk.ExtractResult {
	return e.ExtractContext(context.Background(), d)
}

// ExtractContext is run in Hister's enrichment phase. A successful download
// does not claim content extraction, so the later ytdlp/readability extractors
// still populate the document and preview.
func (e *YTDownloader) ExtractContext(ctx context.Context, d *sdk.Document) sdk.ExtractResult {
	if !e.GetConfig().Enable {
		return sdk.ExtractFallback(nil)
	}
	options, err := e.downloaderOptions()
	if err != nil {
		return sdk.ExtractFallback(err)
	}
	if handled, err := medialifecycle.Stage(ctx, medialifecycle.Request{
		URL: d.URL, UserID: d.UserID, Binary: options.binary, OutputRoot: options.outputRoot,
		Backend: options.backend, Transcribe: options.transcribe, Timeout: options.timeout,
	}); handled {
		if err != nil {
			return sdk.ExtractFallback(err)
		}
		return sdk.Extracted()
	}
	// Standalone extraction retains the producer-protocol client behavior.
	// Indexing always supplies a stager and never publishes these references.
	publicationKey, publicationID, finishPublication := e.beginPublication(d)
	defer finishPublication()
	media, err, joined := e.fetchSingleflight(ctx, d, options)
	if err != nil {
		if ctx.Err() != nil {
			return sdk.AbortExtraction(ctx.Err())
		}
		return sdk.ExtractFallback(err)
	}
	if err := ctx.Err(); err != nil {
		return sdk.AbortExtraction(err)
	}
	references := mediaReferences{Video: fileURIs(media.VideoPaths), Audio: fileURIs(media.AudioPaths)}
	encoded, err := json.Marshal(references)
	if err != nil {
		return sdk.ExtractFallback(fmt.Errorf("encode downloaded media references: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return sdk.AbortExtraction(err)
	}
	// Publish only after both invocations have completed and every reported path
	// has been validated. On any failure the document's previous references stay intact.
	published, err := e.publishReferences(d, publicationKey, publicationID, string(encoded))
	if err != nil {
		return sdk.ExtractFallback(err)
	}
	if !published {
		return sdk.ExtractFallback(errors.New("discarded stale YouTube media result; a newer download was already accepted"))
	}
	message := "downloaded YouTube media"
	if joined {
		message = "reused in-flight YouTube media download"
	}
	log.Info().Str("url", d.URL).Strs("video_paths", media.VideoPaths).Strs("audio_paths", media.AudioPaths).Strs("transcript_paths", media.TranscriptPaths).Msg(message)
	return sdk.Extracted()
}

func (e *YTDownloader) beginPublication(d *sdk.Document) (string, uint64, func()) {
	keyBytes, _ := json.Marshal(struct {
		UserID     uint
		DocumentID string
		URL        string
	}{d.UserID, d.DocumentID, d.URL})
	key := string(keyBytes)
	e.publicationMu.Lock()
	if e.publications == nil {
		e.publications = make(map[string]*publicationState)
	}
	state := e.publications[key]
	if state == nil {
		state = &publicationState{}
		e.publications[key] = state
	}
	e.nextPublicationID++
	publicationID := e.nextPublicationID
	state.active++
	e.publicationMu.Unlock()
	return key, publicationID, func() {
		e.publicationMu.Lock()
		defer e.publicationMu.Unlock()
		if current := e.publications[key]; current != nil {
			current.active--
			if current.active == 0 {
				delete(e.publications, key)
			}
		}
	}
}

func (e *YTDownloader) publishReferences(d *sdk.Document, key string, publicationID uint64, encoded string) (bool, error) {
	e.publicationMu.Lock()
	defer e.publicationMu.Unlock()
	state := e.publications[key]
	if state == nil {
		return false, errors.New("YouTube media publication state expired unexpectedly")
	}
	if publicationID < state.acceptedPublication {
		// Keep a stale document snapshot from carrying old paths back into the
		// indexer if it is published after the newer successful enrichment.
		if d.Metadata == nil {
			d.Metadata = make(map[string]any)
		}
		d.Metadata[MediaReferencesMetadataKey] = state.acceptedReferences
		return false, nil
	}
	state.acceptedPublication = publicationID
	state.acceptedReferences = encoded
	if d.Metadata == nil {
		d.Metadata = make(map[string]any)
	}
	d.Metadata[MediaReferencesMetadataKey] = encoded
	return true, nil
}

func fileURIs(paths []string) []string {
	uris := make([]string, 0, len(paths))
	for _, path := range paths {
		uris = append(uris, files.PathToFileURL(path))
	}
	return uris
}

type mediaReferences struct {
	Video []string `json:"video"`
	Audio []string `json:"audio"`
}

type downloaderOptions struct {
	binary     string
	outputRoot string
	backend    string
	transcribe bool
	timeout    int
}

func (e *YTDownloader) downloaderOptions() (downloaderOptions, error) {
	cfg := e.GetConfig()
	seconds, err := integerOptionDefault(cfg.Options, "timeout", 1800)
	if err != nil {
		return downloaderOptions{}, err
	}
	return downloaderOptions{
		binary:     stringOption(cfg.Options, "binary", defaultBinary),
		outputRoot: files.ExpandHome(stringOption(cfg.Options, "output_dir", defaultOutputDir)),
		backend:    stringOption(cfg.Options, "backend", "auto"),
		transcribe: cfg.Options["transcribe"] == true,
		timeout:    seconds,
	}, nil
}

type downloadSource struct {
	documentID string
	userID     uint
	url        string
	videoID    string
}

// A leader owns a downloader process. A waiting caller may abandon its wait
// without canceling the leader; if the leader is canceled, all waiters observe
// that failure and a later request starts a fresh job.
func (e *YTDownloader) fetchSingleflight(ctx context.Context, d *sdk.Document, options downloaderOptions) (*downloadResult, error, bool) {
	videoID, err := youtubeVideoID(d.URL)
	if err != nil {
		return nil, err, false
	}
	source := downloadSource{documentID: d.DocumentID, userID: d.UserID, url: d.URL, videoID: videoID}
	keyBytes, _ := json.Marshal(struct {
		VideoID    string
		UserID     uint
		Binary     string
		OutputRoot string
		Backend    string
		Transcribe bool
		Timeout    int
	}{videoID, d.UserID, options.binary, options.outputRoot, options.backend, options.transcribe, options.timeout})
	key := string(keyBytes)

	e.downloadsMu.Lock()
	if e.downloads == nil {
		e.downloads = make(map[string]*downloadCall)
	}
	if call, ok := e.downloads[key]; ok {
		e.downloadsMu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	}
	call := &downloadCall{done: make(chan struct{})}
	e.downloads[key] = call
	e.downloadsMu.Unlock()

	call.result, call.err = e.fetch(ctx, source, options)
	e.downloadsMu.Lock()
	delete(e.downloads, key)
	close(call.done)
	e.downloadsMu.Unlock()
	return call.result, call.err, false
}

func youtubeVideoID(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", fmt.Errorf("unsupported YouTube video URL %q", rawURL)
	}
	host := strings.ToLower(parsed.Hostname())
	isYouTube := host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
	isNoCookie := host == "youtube-nocookie.com" || strings.HasSuffix(host, ".youtube-nocookie.com")
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	var id string
	switch {
	case host == "youtu.be":
		if len(parts) == 1 {
			id = parts[0]
		}
	case isYouTube && len(parts) == 1 && parts[0] == "watch":
		values := parsed.Query()["v"]
		if len(values) == 1 {
			id = values[0]
		}
	case (isYouTube || isNoCookie) && len(parts) == 2:
		switch parts[0] {
		case "shorts", "live", "embed", "v":
			id = parts[1]
		}
	}
	if !validYouTubeVideoID(id) {
		return "", fmt.Errorf("YouTube URL %q does not identify a supported video", rawURL)
	}
	return id, nil
}

func validYouTubeVideoID(id string) bool {
	if len(id) != 11 {
		return false
	}
	for _, character := range id {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_' || character == '-':
		default:
			return false
		}
	}
	return true
}

// Preview reads server-owned state; submitted/imported metadata never creates
// playback links. The frontend polls the scoped read-only API while active.
func (e *YTDownloader) Preview(d *sdk.Document) sdk.PreviewResult {
	status, err := medialifecycle.ReadStatus(d.ID(), d.UserID)
	if err != nil {
		return sdk.Previewed(sdk.PreviewResponse{Content: "<p>Media status is unavailable. No download readiness could be verified.</p>"})
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return sdk.PreviewFallback(err)
	}
	return sdk.Previewed(sdk.PreviewResponse{Template: "yt_downloader", Content: string(encoded)})
}

type downloadResult struct {
	VideoPaths      []string
	AudioPaths      []string
	TranscriptPaths []string
}

type downloaderInvocation struct {
	jobID             string
	sourceDocumentID  string
	userID            uint
	requestedURL      string
	expectedVideoID   string
	jobType           string
	options           downloaderOptions
	outputRoot        string
	expectedOutputDir string
	artifactsRoot     string
	resultFile        string
	context           context.Context
}

type downloaderManifest struct {
	SchemaVersion   int      `json:"schema_version"`
	JobID           string   `json:"job_id"`
	JobType         string   `json:"job_type"`
	URL             string   `json:"url"`
	VideoID         string   `json:"video_id"`
	OutputDir       string   `json:"output_dir"`
	Status          string   `json:"status"`
	MediaPaths      []string `json:"media_paths"`
	TranscriptPaths []string `json:"transcript_paths"`
	Error           *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *YTDownloader) fetch(parent context.Context, source downloadSource, options downloaderOptions) (*downloadResult, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(options.timeout)*time.Second)
	defer cancel()

	result := &downloadResult{}
	err := e.runJob(ctx, func() error {
		videoType := "scrape_videos"
		videoManifest, err := runDownloaderInvocation(ctx, source, options, "download", videoType)
		if err != nil {
			return err
		}
		result.VideoPaths = videoManifest.MediaPaths
		result.TranscriptPaths = videoManifest.TranscriptPaths

		audioManifest, err := runDownloaderInvocation(ctx, source, options, "audio", "scrape_audio_only")
		if err != nil {
			return err
		}
		result.AudioPaths = audioManifest.MediaPaths
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func runDownloaderInvocation(ctx context.Context, source downloadSource, options downloaderOptions, command, jobType string) (*downloaderManifest, error) {
	return runDownloaderInvocationWithEvents(ctx, source, options, command, jobType, nil, nil, nil)
}

func runDownloaderInvocationWithEvents(ctx context.Context, source downloadSource, options downloaderOptions, command, jobType string, prepared func(string) error, started, completed func() error) (*downloaderManifest, error) {
	invocation, err := newDownloaderInvocation(ctx, source, options, jobType)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.Remove(invocation.resultFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn().Err(err).Str("result_file", invocation.resultFile).Msg("could not remove temporary downloader result manifest")
		}
	}()

	args := []string{
		command, invocation.requestedURL,
		"--job-id", invocation.jobID,
		"--output-root", invocation.outputRoot,
		"--artifacts-root", invocation.artifactsRoot,
		"--result-json", invocation.resultFile,
		"--backend", options.backend,
		"--no-playlist",
	}
	if command == "download" && options.transcribe {
		args = append(args,
			"--video-owner-transcript", "--yt-auto-transcript",
			"--transcript-langs", "en,en-orig", "--transcript-format", "vtt",
		)
	}
	if prepared != nil {
		if err := prepared(invocation.jobID); err != nil {
			return nil, err
		}
	}
	if _, err := runDownloaderCommandWithStart(ctx, options.binary, started, args...); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if completed != nil {
		if err := completed(); err != nil {
			return nil, err
		}
	}
	manifestBytes, err := readResultManifest(invocation.resultFile)
	if err != nil {
		return nil, fmt.Errorf("%w: yt_downloader did not publish a usable result manifest (%s): %w", medialifecycle.ErrInvalidOutput, manifestUpgradeHint, err)
	}
	manifest, mediaPaths, transcriptPaths, err := validateResultManifest(manifestBytes, invocation)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", medialifecycle.ErrInvalidOutput, err)
	}
	manifest.MediaPaths = mediaPaths
	manifest.TranscriptPaths = transcriptPaths
	return manifest, nil
}

func newDownloaderInvocation(ctx context.Context, source downloadSource, options downloaderOptions, jobType string) (*downloaderInvocation, error) {
	outputRoot, err := canonicalOutputRoot(options.outputRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare YouTube media output root: %w", err)
	}
	stateRoot, err := downloaderStateRoot()
	if err != nil {
		return nil, fmt.Errorf("prepare YouTube downloader state directory: %w", err)
	}

	var jobID string
	var artifactsRoot string
	var resultFile string
	var expectedOutputDir string
	for range 3 {
		jobID, err = newJobID(jobType)
		if err != nil {
			return nil, err
		}
		expectedOutputDir = filepath.Join(outputRoot, jobID)
		resultFile = filepath.Join(stateRoot, jobID+".result.json")
		if _, err := os.Lstat(expectedOutputDir); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check downloader job output directory: %w", err)
		}
		if _, err := os.Lstat(resultFile); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check downloader result path: %w", err)
		}
		artifactsRoot = filepath.Join(stateRoot, "artifacts", jobID)
		if err := os.MkdirAll(filepath.Dir(artifactsRoot), 0o700); err != nil {
			return nil, fmt.Errorf("create downloader artifacts root: %w", err)
		}
		if err := os.Mkdir(artifactsRoot, 0o700); err == nil {
			break
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create downloader artifacts directory: %w", err)
		}
		artifactsRoot = ""
	}
	if artifactsRoot == "" {
		return nil, errors.New("could not allocate a unique downloader job directory")
	}
	return &downloaderInvocation{
		jobID:             jobID,
		sourceDocumentID:  source.documentID,
		userID:            source.userID,
		requestedURL:      source.url,
		expectedVideoID:   source.videoID,
		jobType:           jobType,
		options:           options,
		outputRoot:        outputRoot,
		expectedOutputDir: expectedOutputDir,
		artifactsRoot:     artifactsRoot,
		resultFile:        resultFile,
		context:           ctx,
	}, nil
}

func canonicalOutputRoot(configuredRoot string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(files.ExpandHome(configuredRoot)))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", root)
	}
	return root, nil
}

func downloaderStateRoot() (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	// Preserve the existing cache/report location across the integration rename.
	root := filepath.Join(cacheRoot, "hister", "yt_video_downloader")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("downloader state root is not a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

func newJobID(jobType string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create unique downloader job ID: %w", err)
	}
	mode := "video"
	if jobType == "scrape_audio_only" {
		mode = "audio"
	}
	return "hister-" + hex.EncodeToString(random[:]) + "-" + mode, nil
}

func runDownloaderCommand(ctx context.Context, binary string, args ...string) (string, error) {
	return runDownloaderCommandWithStart(ctx, binary, nil, args...)
}

func runDownloaderCommandWithStart(ctx context.Context, binary string, started func() error, args ...string) (string, error) {
	var stdout cappedBuffer
	stdout.limit = maxDiagnosticSize
	var stderr cappedBuffer
	stderr.limit = maxDiagnosticSize
	cmd := exec.CommandContext(ctx, binary, args...)
	configureDownloaderProcess(cmd)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Start()
	if err == nil {
		if started != nil {
			if startErr := started(); startErr != nil {
				_ = cmd.Cancel()
				_ = cmd.Wait()
				return "", startErr
			}
		}
		err = cmd.Wait()
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("yt_downloader %s failed (client %s): %w: %s", args[0], manifestUpgradeHint, err, detail)
		}
		return "", fmt.Errorf("yt_downloader %s failed (client %s): %w", args[0], manifestUpgradeHint, err)
	}
	return stdout.String(), nil
}

func readResultManifest(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("result path is not a regular file")
	}
	if info.Size() > maxManifestSize {
		return nil, fmt.Errorf("result manifest exceeds %d bytes", maxManifestSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return nil, errors.New("result file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("result manifest exceeds %d bytes", maxManifestSize)
	}
	return data, nil
}

func validateResultManifest(data []byte, invocation *downloaderInvocation) (*downloaderManifest, []string, []string, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid yt_downloader result manifest: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("top-level JSON value must be an object")
		}
		return nil, nil, nil, fmt.Errorf("invalid yt_downloader result manifest: %w", err)
	}
	for _, required := range []string{"schema_version", "job_id", "job_type", "url", "video_id", "output_dir", "status", "media_paths", "transcript_paths", "error"} {
		if _, ok := fields[required]; !ok {
			return nil, nil, nil, fmt.Errorf("invalid yt_downloader result manifest: missing required field %q", required)
		}
	}
	var manifest downloaderManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid yt_downloader result manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return nil, nil, nil, fmt.Errorf("unsupported yt_downloader result schema version %d (%s)", manifest.SchemaVersion, manifestUpgradeHint)
	}
	if manifest.JobID != invocation.jobID || manifest.JobType != invocation.jobType || manifest.URL != invocation.requestedURL {
		return nil, nil, nil, errors.New("yt_downloader result does not match the requested job ID, URL, or mode")
	}
	if !filepath.IsAbs(manifest.OutputDir) || filepath.Clean(manifest.OutputDir) != manifest.OutputDir || manifest.OutputDir != invocation.expectedOutputDir {
		return nil, nil, nil, errors.New("yt_downloader result has an unexpected or non-canonical output directory")
	}
	if manifest.MediaPaths == nil || manifest.TranscriptPaths == nil {
		return nil, nil, nil, errors.New("yt_downloader result media_paths and transcript_paths must be arrays")
	}

	switch manifest.Status {
	case "completed":
		if manifest.Error != nil {
			return nil, nil, nil, errors.New("completed yt_downloader result must have error: null")
		}
		if manifest.VideoID != invocation.expectedVideoID || !validYouTubeVideoID(manifest.VideoID) {
			return nil, nil, nil, errors.New("yt_downloader result video ID does not match the requested YouTube video")
		}
	case "failed", "unavailable", "cancelled":
		if manifest.Error == nil || strings.TrimSpace(manifest.Error.Code) == "" || strings.TrimSpace(manifest.Error.Message) == "" {
			return nil, nil, nil, fmt.Errorf("yt_downloader reported %q without a valid error object", manifest.Status)
		}
		return nil, nil, nil, fmt.Errorf("yt_downloader job %s: %s: %s", manifest.Status, manifest.Error.Code, manifest.Error.Message)
	default:
		return nil, nil, nil, fmt.Errorf("yt_downloader result has unknown status %q", manifest.Status)
	}
	if err := validateExpectedOutputDir(invocation); err != nil {
		return nil, nil, nil, err
	}
	mediaPaths, err := validateInvocationPaths(manifest.MediaPaths, invocation)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid required media_paths: %w", err)
	}
	if len(mediaPaths) == 0 {
		return nil, nil, nil, errors.New("completed yt_downloader result contains no media paths")
	}
	transcriptPaths, err := validateInvocationPaths(manifest.TranscriptPaths, invocation)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid transcript_paths: %w", err)
	}
	return &manifest, mediaPaths, transcriptPaths, nil
}

func validateExpectedOutputDir(invocation *downloaderInvocation) error {
	resolved, err := filepath.EvalSymlinks(invocation.expectedOutputDir)
	if err != nil {
		return fmt.Errorf("expected job output directory is unavailable: %w", err)
	}
	if resolved != invocation.expectedOutputDir {
		return errors.New("expected job output directory resolves through a symlink")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return errors.New("expected job output path is not a directory")
	}
	return nil
}

func validateInvocationPaths(paths []string, invocation *downloaderInvocation) ([]string, error) {
	validated := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, rawPath := range paths {
		if !filepath.IsAbs(rawPath) || filepath.Clean(rawPath) != rawPath || !withinDirectory(invocation.expectedOutputDir, rawPath) || !withinDirectory(invocation.outputRoot, rawPath) {
			return nil, fmt.Errorf("path %q is not a canonical absolute path inside this job directory", rawPath)
		}
		resolved, err := filepath.EvalSymlinks(rawPath)
		if err != nil {
			return nil, fmt.Errorf("resolve path %q: %w", rawPath, err)
		}
		if !withinDirectory(invocation.expectedOutputDir, resolved) || !withinDirectory(invocation.outputRoot, resolved) {
			return nil, fmt.Errorf("path %q resolves outside this job directory", rawPath)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("path %q is not a non-empty regular file", rawPath)
		}
		if _, duplicate := seen[resolved]; duplicate {
			continue
		}
		seen[resolved] = struct{}{}
		validated = append(validated, resolved)
	}
	return validated, nil
}

func withinDirectory(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && filepath.IsLocal(relative)
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func (e *YTDownloader) runJob(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	jobSlots := e.jobSlots
	if jobSlots == nil {
		return run()
	}
	select {
	case jobSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-jobSlots }()
	return run()
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = b.Buffer.Write(data[:remaining])
			b.truncated = true
		} else {
			_, _ = b.Buffer.Write(data)
		}
	} else if len(data) > 0 {
		b.truncated = true
	}
	return len(data), nil
}

func (b *cappedBuffer) String() string {
	value := b.Buffer.String()
	if b.truncated {
		return value + "\n[diagnostic output truncated]"
	}
	return value
}

func stringOption(options map[string]any, key, fallback string) string {
	if value, ok := options[key].(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func integerOptionDefault(options map[string]any, key string, fallback int) (int, error) {
	if value, ok := options[key]; ok {
		return integerOption(value, key)
	}
	return fallback, nil
}
