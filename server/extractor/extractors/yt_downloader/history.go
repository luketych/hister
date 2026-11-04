// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
)

// HistoryPlan is an offline evidence inventory, NOT a migration or playback
// credential. Operational reports are unsigned and do not identify a Hister
// owner. Even mutually consistent reports require explicit ownership/pairing
// approval and a fresh live-source/registry check before any future import.
type HistoryPlan struct {
	SchemaVersion     int             `json:"schema_version"`
	DryRun            bool            `json:"dry_run"`
	RegistryChecked   bool            `json:"registry_checked"`
	OwnershipVerified bool            `json:"ownership_verified"`
	Jobs              []HistoryJob    `json:"jobs"`
	Sources           []HistorySource `json:"sources"`
}

type HistoryJob struct {
	JobID          string       `json:"job_id"`
	Role           string       `json:"role"`
	State          string       `json:"state"`
	Code           string       `json:"code,omitempty"`
	SourceURL      string       `json:"source_url,omitempty"`
	VideoID        string       `json:"video_id,omitempty"`
	EvidenceSHA256 string       `json:"evidence_sha256,omitempty"`
	File           *HistoryFile `json:"file,omitempty"`
}

type HistoryFile struct {
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
}

type HistorySource struct {
	VideoID   string   `json:"video_id"`
	State     string   `json:"state"`
	VideoJobs []string `json:"video_jobs"`
	AudioJobs []string `json:"audio_jobs"`
}

type historyFileRecord struct {
	Path string `json:"path"`
	Size int64  `json:"bytes"`
}

type historyReport struct {
	JobID           string              `json:"job_id"`
	OutputDir       string              `json:"output_dir"`
	ArtifactsDir    string              `json:"artifacts_dir"`
	RequestedURLs   []string            `json:"requested_urls"`
	Status          string              `json:"status"`
	ErrorCode       *string             `json:"error_code"`
	DeliveredFiles  []historyFileRecord `json:"delivered_files"`
	ProvenanceFiles []historyFileRecord `json:"provenance_files"`
	Items           []struct {
		URL             string              `json:"url"`
		Status          string              `json:"status"`
		JobType         string              `json:"job_type"`
		VideoID         string              `json:"video_id"`
		MediaPaths      []string            `json:"media_paths"`
		SavedFiles      []historyFileRecord `json:"saved_files"`
		ProvenanceFiles []historyFileRecord `json:"provenance_files"`
	} `json:"items"`
}

type historyProvenance struct {
	SchemaVersion int `json:"schema_version"`
	Source        struct {
		URL     string `json:"url"`
		VideoID string `json:"video_id"`
	} `json:"source"`
	Retrieval struct {
		JobType string `json:"job_type"`
	} `json:"retrieval"`
	Outputs []struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	} `json:"outputs"`
}

// PlanHistory reads only explicitly named Hister jobs from the fixed retained
// report layout. No config/runtime/SQL/index initialization, directory discovery,
// external process, download, registration, cleanup or filesystem write occurs.
// This first slice deliberately has no apply mode or owner-mapping parameter.
func PlanHistory(ctx context.Context, stateDir, mediaRoot string, jobIDs []string) (*HistoryPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(jobIDs) == 0 || len(jobIDs) > 1000 {
		return nil, errors.New("select between 1 and 1000 explicit Hister job IDs")
	}
	ids := slices.Clone(jobIDs)
	slices.Sort(ids)
	for n, id := range ids {
		if historyJobRole(id) == "" || (n > 0 && id == ids[n-1]) {
			return nil, errors.New("job IDs must be distinct hister-<32 lowercase hex digits>-video/audio names")
		}
	}
	state, err := historyRoot(stateDir)
	if err != nil {
		return nil, errors.New("state directory must name an existing directory")
	}
	root, err := historyRoot(mediaRoot)
	if err != nil {
		return nil, errors.New("media root must name an existing directory")
	}
	plan := &HistoryPlan{SchemaVersion: 1, DryRun: true, Jobs: []HistoryJob{}, Sources: []HistorySource{}}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plan.Jobs = append(plan.Jobs, inspectHistoryJob(state, root, id))
	}
	// Group possible roles by exact confirmed video identity, but never infer
	// a pair, current selection or owner from titles, timestamps or job ordering.
	for _, job := range plan.Jobs {
		if job.State != "owner_approval_required" {
			continue
		}
		n := slices.IndexFunc(plan.Sources, func(source HistorySource) bool { return source.VideoID == job.VideoID })
		if n < 0 {
			plan.Sources = append(plan.Sources, HistorySource{VideoID: job.VideoID, VideoJobs: []string{}, AudioJobs: []string{}})
			n = len(plan.Sources) - 1
		}
		if job.Role == model.MediaVideo {
			plan.Sources[n].VideoJobs = append(plan.Sources[n].VideoJobs, job.JobID)
		} else {
			plan.Sources[n].AudioJobs = append(plan.Sources[n].AudioJobs, job.JobID)
		}
	}
	slices.SortFunc(plan.Sources, func(a, b HistorySource) int { return strings.Compare(a.VideoID, b.VideoID) })
	for n := range plan.Sources {
		source := &plan.Sources[n]
		source.State = "pair_approval_required"
		if len(source.VideoJobs) == 0 || len(source.AudioJobs) == 0 {
			source.State = "incomplete_pair"
		} else if len(source.VideoJobs) > 1 || len(source.AudioJobs) > 1 {
			source.State = "ambiguous_pair"
		}
	}
	return plan, nil
}

func historyJobRole(id string) string {
	for _, role := range []string{model.MediaVideo, model.MediaAudio} {
		random := strings.TrimSuffix(strings.TrimPrefix(id, "hister-"), "-"+role)
		if len(random) != 32 || id != "hister-"+random+"-"+role || strings.ToLower(random) != random {
			continue
		}
		if _, err := hex.DecodeString(random); err == nil {
			return role
		}
	}
	return ""
}

func historyRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty directory")
	}
	absolute, err := filepath.Abs(files.ExpandHome(path))
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return root, nil
}

// Preserve only controlled error codes in the plan. Raw errors, absolute paths,
// backend diagnostics and URL credentials are omitted. Candidate filenames
// remain relative to the explicitly selected media root.
func inspectHistoryJob(state, root, id string) HistoryJob {
	job := HistoryJob{JobID: id, Role: historyJobRole(id), State: "blocked", Code: "reports_unavailable"}
	reportDir := filepath.Join("artifacts", id, "jobs", id)
	expectedOutput := filepath.Join(root, id)
	var request, result, manifest historyReport
	digest := sha256.New()
	for _, report := range []struct {
		name  string
		value *historyReport
	}{{"request.json", &request}, {"result.json", &result}, {"manifest.json", &manifest}} {
		data, err := readHistoryJSON(state, filepath.Join(reportDir, report.name), report.value)
		if err != nil {
			return job
		}
		_, _ = digest.Write(data)
		if report.value.JobID != id || report.value.OutputDir != expectedOutput {
			job.Code = "report_identity_mismatch"
			return job
		}
	}
	job.Code = "source_or_mode_unconfirmed"
	if request.ArtifactsDir != filepath.Join(state, reportDir) || len(request.RequestedURLs) != 1 || !reflect.DeepEqual(request.RequestedURLs, result.RequestedURLs) || len(result.Items) != 1 {
		return job
	}
	if result.Status != "success" || (result.ErrorCode != nil && *result.ErrorCode != "") {
		job.Code = "job_not_successful"
		return job
	}
	item := result.Items[0]
	jobType := "scrape_videos"
	if job.Role == model.MediaAudio {
		jobType = "scrape_audio_only"
	}
	videoID, err := youtubeVideoID(request.RequestedURLs[0])
	if err != nil || item.URL != request.RequestedURLs[0] || item.Status != "success" || item.JobType != jobType || item.VideoID != videoID {
		return job
	}
	job.Code = "outputs_unconfirmed"
	if len(item.MediaPaths) != 1 || len(manifest.DeliveredFiles) != 1 || len(item.SavedFiles) != 1 || item.MediaPaths[0] != manifest.DeliveredFiles[0].Path || !reflect.DeepEqual(item.SavedFiles, manifest.DeliveredFiles) {
		return job
	}
	output := manifest.DeliveredFiles[0]
	relative, ok := historyJobPath(root, id, output.Path)
	contentType := playbackContentType(job.Role, output.Path)
	if !ok || output.Size <= 0 || contentType == "" {
		return job
	}
	file, _, err := media.OpenArtifact(model.MediaArtifactInput{StorageRoot: root, RelativePath: relative, Size: output.Size})
	if err != nil {
		job.Code = "media_unavailable"
		return job
	}
	_ = file.Close()
	job.Code = "provenance_unconfirmed"
	if len(manifest.ProvenanceFiles) != 1 || !reflect.DeepEqual(manifest.ProvenanceFiles, result.ProvenanceFiles) || !reflect.DeepEqual(manifest.ProvenanceFiles, item.ProvenanceFiles) {
		return job
	}
	provenanceRecord := manifest.ProvenanceFiles[0]
	provenanceRelative, ok := historyJobPath(root, id, provenanceRecord.Path)
	if !ok || provenanceRecord.Size <= 0 {
		return job
	}
	var provenance historyProvenance
	data, err := readHistoryJSON(root, provenanceRelative, &provenance)
	if err != nil || int64(len(data)) != provenanceRecord.Size {
		return job
	}
	_, _ = digest.Write(data)
	provenanceID, err := youtubeVideoID(provenance.Source.URL)
	if err != nil || provenance.SchemaVersion != 1 || provenance.Source.VideoID != videoID || provenanceID != videoID || provenance.Retrieval.JobType != jobType {
		return job
	}
	mediaOutputs := 0
	for _, outputRecord := range provenance.Outputs {
		if !filepath.IsLocal(outputRecord.Path) || filepath.Clean(outputRecord.Path) != outputRecord.Path {
			return job
		}
		if outputRecord.Kind != "media" {
			continue
		}
		if filepath.Join(filepath.Dir(provenanceRecord.Path), outputRecord.Path) != output.Path {
			return job
		}
		mediaOutputs++
	}
	if mediaOutputs != 1 {
		return job
	}
	job.State, job.Code = "owner_approval_required", ""
	job.SourceURL, job.VideoID = "https://www.youtube.com/watch?v="+videoID, videoID
	job.EvidenceSHA256 = hex.EncodeToString(digest.Sum(nil))
	job.File = &HistoryFile{RelativePath: relative, Size: output.Size, ContentType: contentType}
	return job
}

func historyJobPath(root, id, path string) (string, bool) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	relative, err := filepath.Rel(root, path)
	return relative, err == nil && filepath.IsLocal(relative) && strings.HasPrefix(relative, id+string(filepath.Separator))
}

func readHistoryJSON(root, relative string, value any) ([]byte, error) {
	info, err := os.Lstat(filepath.Join(root, relative))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxManifestSize {
		return nil, errors.New("report must be a bounded regular file")
	}
	file, _, err := media.OpenArtifact(model.MediaArtifactInput{StorageRoot: root, RelativePath: relative, Size: info.Size()})
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestSize {
		return nil, errors.New("report exceeds size limit")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("report must contain a JSON object")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, err
	}
	return data, nil
}
