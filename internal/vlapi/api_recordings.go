package vlapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// VODController is implemented by the daemon to serve the VOD Archive: listing
// VODs, producing thumbnails, and deleting VOD folders. Controller errors
// wrapping ErrNotFound/ErrConflict/errInvalidArg map onto the matching API
// error codes.
type VODController interface {
	// ListVODs returns the current VOD Archive listing (cached scan).
	ListVODs(ctx context.Context) vodindex.Result
	// RescanVODs forces the next ListVODs to reconcile against the filesystem,
	// picking up out-of-band edits the daemon didn't observe (Finder deletes,
	// externally-copied files) without waiting for the TTL. It returns
	// immediately; the walk runs in the background and bumps the rev when done.
	RescanVODs(ctx context.Context)
	// VODThumbnail returns the JPEG screenshot for one file of a listed VOD,
	// generating and caching it when missing.
	VODThumbnail(ctx context.Context, dir, file string) ([]byte, error)
	// VODPreviews returns the layout of a VOD file's timeline previews and
	// its hover strip.
	VODPreviews(ctx context.Context, dir, file string) (processing.PreviewsManifest, []byte, error)
	// VODContactSheet returns the contact sheet across all of a VOD's files,
	// building it on first request.
	VODContactSheet(ctx context.Context, dir string) ([]byte, error)
	// DeleteVOD removes a listed VOD's folder from disk.
	DeleteVOD(ctx context.Context, dir string) error
	// CancelMoves retires pending moves and returns how many it retired. A nil
	// key cancels every pending move; otherwise only that streamer's. Files
	// already moved stay there, so a part-moved VOD is left split across the
	// two folders.
	CancelMoves(ctx context.Context, key *streamer.StreamerKey) (int, error)
}

// vodListDefaultLimit and vodListMaxLimit cap VODs per list page; clients page
// through with offset/limit.
const (
	vodListDefaultLimit = 500
	vodListMaxLimit     = 1000
)

// vodListMaxBytes caps a page by its serialized size, since NATS limits
// payloads to 1 MiB and a VOD's size varies with its file count. Half the limit
// leaves room for the rest of the response. Clients page by how many VODs they
// received, so a short page just costs one more request.
const vodListMaxBytes = 512 * 1024

type vodListRequest struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	// Rescan (honoured only on the first page, offset 0) forces a filesystem
	// reconcile before serving — the manual-refresh path. Serves the current
	// cache immediately; the walk runs in the background.
	Rescan bool `json:"rescan"`
}

type vodListPage struct {
	Sessions []vodindex.VOD `json:"sessions"`
	// Total sessions across all pages, for the client's paging loop.
	Total            int       `json:"total"`
	Offset           int       `json:"offset"`
	UnavailableRoots []string  `json:"unavailable_roots"`
	ScannedAt        time.Time `json:"scanned_at"`
	// Rev is the listing's content generation (see vodindex.Result.Rev),
	// serialized as a decimal string because it's a unix-nanosecond int64 that
	// exceeds JS's safe-integer range. Clients skip re-downloading every page
	// when it hasn't moved.
	Rev string `json:"rev,omitempty"`
	// Scanning is true when a background rescan is in flight and this listing
	// may be stale or empty (see vodindex.Result.Scanning).
	Scanning bool `json:"scanning"`
}

type vodThumbnailRequest struct {
	Dir  string `json:"dir"`
	File string `json:"file"`
}

type vodThumbnailResponse struct {
	// JPEG bytes, base64-encoded inside the JSON envelope. Thumbnails are
	// small (~10-50 KB), so the base64 overhead is fine.
	JpegB64 string `json:"jpeg_b64"`
}

// vodPreviewsResponse is a VOD file's preview layout and its hover strip.
type vodPreviewsResponse struct {
	processing.PreviewsManifest
	ScrubB64 string `json:"scrub_b64"`
}

type vodDeleteRequest struct {
	Dir string `json:"dir"`
}

// vodDeleteManyRequest deletes a selection in one call, reporting per entry
// since partial failure is normal (a VOD still moving is refused).
type vodDeleteManyRequest struct {
	Dirs []string `json:"dirs"`
}

// vodDeleteResult is one entry's outcome. Failures carry their reason rather
// than collapsing into a count, so the user is told which VODs survived and why.
type vodDeleteResult struct {
	Dir     string `json:"dir"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error,omitempty"`
}

type vodDeleteManyResponse struct {
	Results []vodDeleteResult `json:"results"`
	Deleted int               `json:"deleted"`
	Failed  int               `json:"failed"`
}

// vodDeleteManyMax bounds one batch. Deleting a VOD folder on a network drive
// can take seconds, so a large batch would outlast the client's request
// timeout.
const vodDeleteManyMax = 50

// fitSessions trims a page to what will fit in a NATS message by measuring each
// VOD. It always keeps at least one, so an oversized VOD fails loudly instead
// of reading as an empty page (the end of the listing).
func fitSessions(sessions []vodindex.VOD) []vodindex.VOD {
	budget := vodListMaxBytes
	for i, s := range sessions {
		b, err := json.Marshal(s)
		if err != nil {
			// Unmarshalable VOD: keep what measured cleanly, and let the
			// page-level marshal report the problem rather than guessing here.
			return sessions[:max(i, 1)]
		}
		budget -= len(b)
		if budget < 0 {
			return sessions[:max(i, 1)]
		}
	}
	return sessions
}

// handleListVODs returns one page of the VOD Archive listing.
func (s *Service) handleListVODs(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodListRequest
	if len(req.Data()) > 0 {
		if err := json.Unmarshal(req.Data(), &r); err != nil {
			respond(req, fail(errInvalidArg, "invalid JSON"))
			return
		}
	}
	if r.Limit <= 0 {
		r.Limit = vodListDefaultLimit
	}
	if r.Limit > vodListMaxLimit {
		r.Limit = vodListMaxLimit
	}
	if r.Offset < 0 {
		r.Offset = 0
	}

	// A forced rescan only makes sense on the first page: it force-expires the
	// cache so this same ListVODs schedules the reconciling walk and returns
	// the current (stale) contents with Scanning set.
	if r.Rescan && r.Offset == 0 {
		s.vodsCtl.RescanVODs(context.Background())
	}

	res := s.vodsCtl.ListVODs(context.Background())
	page := vodListPage{
		Total:            len(res.Sessions),
		Offset:           r.Offset,
		UnavailableRoots: res.UnavailableRoots,
		ScannedAt:        res.ScannedAt,
		Rev:              strconv.FormatInt(res.Rev, 10),
		Scanning:         res.Scanning,
		Sessions:         []vodindex.VOD{},
	}
	if r.Offset < len(res.Sessions) {
		end := min(r.Offset+r.Limit, len(res.Sessions))
		page.Sessions = fitSessions(res.Sessions[r.Offset:end])
	}
	respond(req, OK(page))
}

// vodThumbnailTimeout bounds on-demand thumbnail generation, which can be slow
// on a network drive. It sits just under the client's
// VODS_THUMBNAIL_TIMEOUT_MS, so ffmpeg never outlives a client that gave up.
const vodThumbnailTimeout = 55 * time.Second

// handleVODThumbnail returns (generating on demand) one file's screenshot. An
// empty file asks for any screenshot of a session still being worked on.
func (s *Service) handleVODThumbnail(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodThumbnailRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.Dir == "" {
		respond(req, fail(errInvalidArg, "dir is required"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), vodThumbnailTimeout)
	defer cancel()
	jpeg, err := s.vodsCtl.VODThumbnail(ctx, r.Dir, r.File)
	if err != nil {
		respondVODError(req, err, "thumbnail unavailable")
		return
	}
	respond(req, OK(vodThumbnailResponse{JpegB64: base64.StdEncoding.EncodeToString(jpeg)}))
}

// vodPreviewsTimeout bounds building a contact sheet. Like thumbnails it sits
// under the client's timeout.
const vodPreviewsTimeout = 55 * time.Second

// handleVODPreviews returns one file's preview layout and hover strip.
func (s *Service) handleVODPreviews(req micro.Request) {
	r, ok := s.vodFileRequest(req)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), vodPreviewsTimeout)
	defer cancel()
	m, strip, err := s.vodsCtl.VODPreviews(ctx, r.Dir, r.File)
	if err != nil {
		respondVODError(req, err, "previews unavailable")
		return
	}
	respond(req, OK(vodPreviewsResponse{PreviewsManifest: m, ScrubB64: base64.StdEncoding.EncodeToString(strip)}))
}

// handleVODContactSheet returns (building on demand) a VOD's contact sheet.
// Sheets are capped well under the message size once base64-encoded.
func (s *Service) handleVODContactSheet(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodDeleteRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.Dir == "" {
		respond(req, fail(errInvalidArg, "dir is required"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), vodPreviewsTimeout)
	defer cancel()
	jpeg, err := s.vodsCtl.VODContactSheet(ctx, r.Dir)
	if err != nil {
		respondVODError(req, err, "contact sheet unavailable")
		return
	}
	respond(req, OK(vodThumbnailResponse{JpegB64: base64.StdEncoding.EncodeToString(jpeg)}))
}

// vodFileRequest parses a {dir, file} request, answering it itself when it
// can't be served.
func (s *Service) vodFileRequest(req micro.Request) (vodThumbnailRequest, bool) {
	var r vodThumbnailRequest
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return r, false
	}
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.Dir == "" || r.File == "" {
		respond(req, fail(errInvalidArg, "dir and file are required"))
		return r, false
	}
	return r, true
}

// handleDeleteVOD removes a whole VOD folder.
func (s *Service) handleDeleteVOD(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodDeleteRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.Dir == "" {
		respond(req, fail(errInvalidArg, "dir is required"))
		return
	}
	if err := s.vodsCtl.DeleteVOD(context.Background(), r.Dir); err != nil {
		respondVODError(req, err, "delete failed")
		return
	}
	respond(req, OK("deleted"))
}

// handleDeleteManyVODs removes a selection of VOD folders, reporting each
// outcome separately. Entries that cannot be deleted (still moving, already
// gone) do not abort the rest.
func (s *Service) handleDeleteManyVODs(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodDeleteManyRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || len(r.Dirs) == 0 {
		respond(req, fail(errInvalidArg, "dirs is required"))
		return
	}
	if len(r.Dirs) > vodDeleteManyMax {
		respond(req, fail(errInvalidArg,
			fmt.Sprintf("too many VODs in one request (%d, max %d)", len(r.Dirs), vodDeleteManyMax)))
		return
	}

	ctx := context.Background()
	resp := vodDeleteManyResponse{Results: make([]vodDeleteResult, 0, len(r.Dirs))}
	for _, dir := range r.Dirs {
		if dir == "" {
			continue
		}
		if err := s.vodsCtl.DeleteVOD(ctx, dir); err != nil {
			resp.Results = append(resp.Results, vodDeleteResult{Dir: dir, Error: err.Error()})
			resp.Failed++
			continue
		}
		resp.Results = append(resp.Results, vodDeleteResult{Dir: dir, Deleted: true})
		resp.Deleted++
	}
	respond(req, OK(resp))
}

// vodCancelMovesRequest scopes a move cancellation. An empty platform and
// username cancels every pending move; both set cancels one streamer's.
type vodCancelMovesRequest struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
}

// vodCancelMovesResponse reports how many queued moves were retired.
type vodCancelMovesResponse struct {
	Cancelled int `json:"cancelled"`
}

// MARK: Cancel Moves
// handleCancelMoves retires queued moves so they stop retrying. Files already
// moved stay there; a part-moved VOD stays split across the Download folder
// and the Archive folder, and cannot be re-queued.
func (s *Service) handleCancelMoves(req micro.Request) {
	if s.vodsCtl == nil {
		respond(req, fail(errInternal, "vod controller unavailable"))
		return
	}
	var r vodCancelMovesRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}

	var key *streamer.StreamerKey
	if r.Platform != "" || r.Username != "" {
		k := streamer.StreamerKey{Platform: r.Platform, Username: r.Username}
		if !k.Valid() {
			respond(req, fail(errInvalidArg, "platform and username are both required to scope a cancel"))
			return
		}
		key = &k
	}

	n, err := s.vodsCtl.CancelMoves(context.Background(), key)
	if err != nil {
		respondVODError(req, err, "cancel moves failed")
		return
	}
	respond(req, OK(vodCancelMovesResponse{Cancelled: n}))
}

// respondVODError maps controller errors onto API error codes, defaulting to
// an internal error with the given prefix.
func respondVODError(req micro.Request, err error, prefix string) {
	code := errInternal
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, os.ErrNotExist):
		code = ErrNotFound
	case errors.Is(err, ErrConflict):
		code = ErrConflict
	case errors.Is(err, errInvalidArg):
		code = errInvalidArg
	}
	respond(req, fail(code, prefix+": "+err.Error()))
}
