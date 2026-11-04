// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
)

const maxHistoryApprovalBytes = 64 << 10

var (
	ErrHistoryApproval = errors.New("invalid historical media approval")
	ErrHistoryEvidence = errors.New("historical media evidence is unavailable or changed; review a fresh inventory")
)

// HistoryApproval is an explicit operator mapping, not an authentication
// credential. Only a separately authorized administrator may use it. It pins
// ONE existing source incarnation and ONE exact historical pair, never the
// current selection. No actor identity, executable, paths or options come from
// this document. OwnerID is a pointer so omitted/null cannot silently mean zero.
type HistoryApproval struct {
	SchemaVersion int                   `json:"schema_version"`
	ApprovalID    string                `json:"approval_id"`
	Action        string                `json:"action"`
	Source        HistoryApprovalSource `json:"source"`
	Video         HistoryApprovalJob    `json:"video"`
	Audio         HistoryApprovalJob    `json:"audio"`
}

type HistoryApprovalSource struct {
	SourceID   string `json:"source_id"`
	DocumentID string `json:"document_id"`
	OwnerID    *uint  `json:"owner_id"`
	URL        string `json:"url"`
}

type HistoryApprovalJob struct {
	JobID          string `json:"job_id"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	MediaSHA256    string `json:"media_sha256"`
}

// DecodeHistoryApproval rejects duplicate/unknown fields, multiple values,
// omitted ownership, unsupported actions/versions and unbounded documents. Error
// details never echo arbitrary document contents, paths or URLs.
func DecodeHistoryApproval(reader io.Reader) (*HistoryApproval, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxHistoryApprovalBytes+1))
	if err != nil || len(data) > maxHistoryApprovalBytes || !utf8.Valid(data) {
		return nil, ErrHistoryApproval
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, ErrHistoryApproval
	}
	// encoding/json matches struct fields case-insensitively. Require exact
	// canonical keys so Source/source or Owner_ID/owner_id cannot override
	// each other or acquire order-dependent meanings in an approval.
	fields, ok := historyApprovalFields(data, "schema_version", "approval_id", "action", "source", "video", "audio")
	if !ok {
		return nil, ErrHistoryApproval
	}
	if _, ok := historyApprovalFields(fields["source"], "source_id", "document_id", "owner_id", "url"); !ok {
		return nil, ErrHistoryApproval
	}
	for _, role := range []string{"video", "audio"} {
		if _, ok := historyApprovalFields(fields[role], "job_id", "evidence_sha256", "media_sha256"); !ok {
			return nil, ErrHistoryApproval
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var approval *HistoryApproval
	if decoder.Decode(&approval) != nil || approval == nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, ErrHistoryApproval
	}
	if err := approval.Validate(); err != nil {
		return nil, err
	}
	return approval, nil
}

func historyApprovalFields(data []byte, names ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != len(names) {
		return nil, false
	}
	for _, name := range names {
		if _, present := fields[name]; !present {
			return nil, false
		}
	}
	return fields, true
}

func (a *HistoryApproval) Validate() error {
	if a == nil || a.SchemaVersion != 1 || a.Action != "retain_history" || !validHistoryApprovalID(a.ApprovalID) {
		return ErrHistoryApproval
	}
	source := a.Source
	if source.OwnerID == nil || source.SourceID == "" || strings.TrimSpace(source.SourceID) != source.SourceID || len(source.SourceID) > 128 || len(source.URL) > 4096 || source.DocumentID != document.GetDocID(*source.OwnerID, source.URL) {
		return ErrHistoryApproval
	}
	if _, err := youtubeVideoID(source.URL); err != nil {
		return ErrHistoryApproval
	}
	if historyJobRole(a.Video.JobID) != model.MediaVideo || historyJobRole(a.Audio.JobID) != model.MediaAudio {
		return ErrHistoryApproval
	}
	for _, job := range []HistoryApprovalJob{a.Video, a.Audio} {
		if !historySHA256(job.EvidenceSHA256) || !historySHA256(job.MediaSHA256) {
			return ErrHistoryApproval
		}
	}
	return nil
}

func validHistoryApprovalID(id string) bool {
	if len(id) == 0 || len(id) > 120 {
		return false
	}
	for _, c := range id {
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		digit := c >= '0' && c <= '9'
		if !letter && !digit && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func historySHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type historyPinnedFile struct {
	file *os.File
	info os.FileInfo
}

// PreparedHistoryApproval keeps the hashed descriptors open until commit.
// This does not lock storage against external writers or create a filesystem/
// SQL transaction. Recheck must run at the source commit boundary; serving also
// reopens and validates retained files. The caller must always Close this value.
type PreparedHistoryApproval struct {
	input     model.MediaHistoryInput
	files     []historyPinnedFile
	sourceURL string
}

// PrepareHistoryApproval re-reads the named evidence, compares approved JSON
// digests and hashes the actual anchored media descriptors (including same-size
// replacements). It never trusts paths copied into an inventory or approval.
// Potentially lengthy hashing happens before the indexer's short coordinator.
func PrepareHistoryApproval(ctx context.Context, stateDir, mediaRoot string, approval *HistoryApproval) (*PreparedHistoryApproval, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := approval.Validate(); err != nil {
		return nil, err
	}
	// Canonicalize and detach caller-owned pointer fields before any I/O.
	encoded, err := json.Marshal(approval)
	if err != nil {
		return nil, ErrHistoryApproval
	}
	approved, err := DecodeHistoryApproval(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	plan, err := PlanHistory(ctx, stateDir, mediaRoot, []string{approved.Video.JobID, approved.Audio.JobID})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrHistoryEvidence
	}
	root, err := historyRoot(mediaRoot)
	if err != nil {
		return nil, ErrHistoryEvidence
	}
	videoID, _ := youtubeVideoID(approved.Source.URL)
	prepared := &PreparedHistoryApproval{
		sourceURL: approved.Source.URL,
		input: model.MediaHistoryInput{
			ApprovalID: approved.ApprovalID, SourceID: approved.Source.SourceID,
			DocumentID: approved.Source.DocumentID, UserID: *approved.Source.OwnerID, ApprovalJSON: string(encoded),
		},
	}
	complete := false
	defer func() {
		if !complete {
			_ = prepared.Close()
		}
	}()
	for _, expected := range []HistoryApprovalJob{approved.Video, approved.Audio} {
		var candidate *HistoryJob
		for n := range plan.Jobs {
			if plan.Jobs[n].JobID == expected.JobID {
				candidate = &plan.Jobs[n]
			}
		}
		if candidate == nil || candidate.State != "owner_approval_required" || candidate.File == nil || candidate.VideoID != videoID || candidate.EvidenceSHA256 != expected.EvidenceSHA256 {
			return nil, ErrHistoryEvidence
		}
		artifact := model.MediaArtifactInput{
			Role: candidate.Role, JobID: candidate.JobID, StorageRoot: root,
			RelativePath: candidate.File.RelativePath, Size: candidate.File.Size, ContentType: candidate.File.ContentType,
		}
		file, info, err := media.OpenArtifact(artifact)
		if err != nil {
			return nil, ErrHistoryEvidence
		}
		prepared.files = append(prepared.files, historyPinnedFile{file: file, info: info})
		digest, err := hashHistoryMedia(ctx, file, artifact.Size)
		if err != nil {
			return nil, err
		}
		after, err := file.Stat()
		if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || digest != expected.MediaSHA256 {
			return nil, ErrHistoryEvidence
		}
		prepared.input.Pair = append(prepared.input.Pair, artifact)
	}
	complete = true
	return prepared, nil
}

func hashHistoryMedia(ctx context.Context, file *os.File, expectedSize int64) (string, error) {
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		size += int64(n)
		if size > expectedSize {
			return "", ErrHistoryEvidence
		}
		_, _ = hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", ErrHistoryEvidence
		}
	}
	if size != expectedSize {
		return "", ErrHistoryEvidence
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Input is server-internal registration data, not a client authorization token.
// Actor identity is supplied separately from an authenticated/local admin caller.
func (p *PreparedHistoryApproval) Input(approvedBy uint) model.MediaHistoryInput {
	input := p.input
	input.ApprovedBy = approvedBy
	input.Pair = append([]model.MediaArtifactInput(nil), input.Pair...)
	return input
}

func (p *PreparedHistoryApproval) SourceURL() string { return p.sourceURL }

// Recheck rejects descriptor/path swaps or intervening size/mtime changes after
// hashing. SQL still rechecks the live binding under its own source lock.
func (p *PreparedHistoryApproval) Recheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.files) != 2 || len(p.input.Pair) != 2 {
		return ErrHistoryEvidence
	}
	for n, artifact := range p.input.Pair {
		file, current, err := media.OpenArtifact(artifact)
		if err != nil {
			return ErrHistoryEvidence
		}
		_ = file.Close()
		held, err := p.files[n].file.Stat()
		before := p.files[n].info
		if err != nil || !os.SameFile(before, current) || !os.SameFile(before, held) || held.Size() != before.Size() || !held.ModTime().Equal(before.ModTime()) {
			return ErrHistoryEvidence
		}
	}
	return nil
}

func (p *PreparedHistoryApproval) Close() error {
	var err error
	for _, pinned := range p.files {
		err = errors.Join(err, pinned.file.Close())
	}
	p.files = nil
	return err
}
