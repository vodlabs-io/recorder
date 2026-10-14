package fmp4

import (
	"bufio"
	"io"
	"sync/atomic"
)

// Sniff wraps the body of a stream requested as FLV. A progressive
// fragmented MP4, which opens with an ftyp box, is remuxed to FLV (see ToFLV);
// any other body is passed through as it is. The body is first read, to tell
// which, on the first Read.
func Sniff(body io.ReadCloser) io.ReadCloser {
	return &sniffer{body: body}
}

type sniffer struct {
	body io.ReadCloser
	// r is what reads come from once the body has been told; Close may run
	// alongside the first Read.
	r atomic.Pointer[io.ReadCloser]
}

func (s *sniffer) Read(p []byte) (int, error) {
	r := s.r.Load()
	if r == nil {
		br := bufio.NewReaderSize(s.body, 64<<10)
		head, _ := br.Peek(8)
		var rc io.ReadCloser = &readCloser{Reader: br, Closer: s.body}
		if len(head) == 8 && string(head[4:8]) == "ftyp" {
			rc = ToFLV(rc)
		}
		s.r.Store(&rc)
		r = &rc
	}
	return (*r).Read(p)
}

func (s *sniffer) Close() error {
	err := s.body.Close()
	if r := s.r.Load(); r != nil {
		(*r).Close()
	}
	return err
}

type readCloser struct {
	io.Reader
	io.Closer
}

// ToFLV remuxes a progressive fragmented MP4 to FLV, on a timeline that
// starts at zero at its first keyframe. Reading ends with io.EOF when src
// does, or with the error that ended it. Close closes src.
func ToFLV(src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(remux(src, pw))
	}()
	return &flvStream{PipeReader: pr, src: src}
}

type flvStream struct {
	*io.PipeReader
	src io.Closer
}

func (s *flvStream) Close() error {
	// Closing the reader first releases a muxer blocked writing to it.
	err := s.PipeReader.Close()
	s.src.Close()
	return err
}

// maxJump is the largest step in a stream's decode times taken as the stream
// running on; a larger one, either way, is a restart of its clock.
const maxJump = 5000

// frameGap is the step placed between the last frame before a clock restart
// and the first after it.
const frameGap = 33

func remux(src io.Reader, w io.Writer) error {
	d := NewDemuxer(src)
	mux := NewFLVMuxer(w)
	started := false
	var base int64
	lastVideo, lastAudio := int64(-1), int64(-1)
	for {
		smp, err := d.Next()
		if err != nil {
			return err
		}
		if !started {
			if !smp.Video || !smp.Key {
				continue
			}
			started = true
			base = -smp.DTS
		}
		ts := base + smp.DTS
		if smp.Video {
			if lastVideo >= 0 && (ts < lastVideo-maxJump || ts > lastVideo+maxJump) {
				// The stream's clock restarted: carry the timeline on from
				// its next keyframe.
				if !smp.Key {
					continue
				}
				base = lastVideo + frameGap - smp.DTS
				ts = base + smp.DTS
			}
			if ts <= lastVideo {
				continue
			}
			if smp.Key {
				// Sends new sequence headers only when the configuration
				// changed.
				if err := mux.SetConfig(ts, smp.AVCC, smp.ASC); err != nil {
					return err
				}
			}
			lastVideo = ts
			if err := mux.WriteVideo(ts, smp.CTS, smp.Key, smp.Data); err != nil {
				return err
			}
			continue
		}
		// Audio from across a clock restart the video has not re-based yet
		// is dropped.
		if ts < 0 || ts <= lastAudio || ts < lastVideo-maxJump || ts > lastVideo+maxJump {
			continue
		}
		lastAudio = ts
		if err := mux.WriteAudio(ts, smp.Data); err != nil {
			return err
		}
	}
}
