// Package preview builds the still images a VOD is browsed by: timeline
// previews (WebVTT sprite sheets for a player's seek bar), a scrub strip for
// hovering an archive card, a vcsi-style contact sheet, and the three-up
// thumbnail of a portrait video.
//
// The embedded ffmpeg is used only to decode, scale and JPEG-encode frames;
// everything else — picking frames, tiling, text, layout and the VTT — is Go.
// Frames are taken from keyframes wherever possible: a keyframe decodes on
// its own, so a preview costs one picture's decode instead of a stream's.
//
// The seam for a future live wall (many recording streams previewed at once,
// at a frame a second or slower) is the same pair of types: a writer hands
// its newest Keyframe to a holder, and the holder asks the shared Decoder for
// a JPEG only while someone is watching. In the recorder the two places a
// writer sees keyframes are flvfix's Sanitizer (onMedia, for every FLV and
// WebM source) and hlsingest's Sink.Segment (whole HLS segments).
package preview
