package flvfix

// Stats accumulates sanitizer counters for logging and the offline repair tool.
type Stats struct {
	bytesIn  int64
	bytesOut int64
	tagsIn   int64
	tagsOut  int64

	restartsConsumed  int64 // in-band FLV file headers swallowed
	scriptsDropped    int64 // repeated onMetaData tags dropped
	dupConfigsDropped int64 // re-sent identical sequence headers dropped

	gapsGlued    int64 // forward timeline gaps removed
	gapMsRemoved int64

	dedupWindows   int64 // replayed (backward-jump) windows deduplicated
	dedupTags      int64
	dedupMsDropped int64

	resets int64 // large backward jumps treated as timeline resets

	desyncs          int64 // byte-stream desyncs recovered from
	desyncBytesDrop  int64
	keyframeWaitTags int64 // video tags dropped while waiting for a keyframe
	preConfigTags    int64 // media tags dropped before decoder config was known
	negativeTsTags   int64 // leading media landing before the part's anchor point
	jitterClamps     int64 // small backward video jitter clamped monotonic
	tsSnapped        int64 // video timestamps moved onto the output grid
	ctsRepaired      int64 // implausible video composition times replaced

	configChanges      int64 // codec config changes (each rotates the output)
	truncatedTailBytes int64 // partial trailing unit dropped at end of input
}

// Since returns the counters accumulated after base. One Sanitizer spans every
// part joined by RotateOutput, so a caller reporting per-output figures
// subtracts the snapshot it took when that output opened.
func (st Stats) Since(base Stats) Stats {
	return Stats{
		bytesIn:            st.bytesIn - base.bytesIn,
		bytesOut:           st.bytesOut - base.bytesOut,
		tagsIn:             st.tagsIn - base.tagsIn,
		tagsOut:            st.tagsOut - base.tagsOut,
		restartsConsumed:   st.restartsConsumed - base.restartsConsumed,
		scriptsDropped:     st.scriptsDropped - base.scriptsDropped,
		dupConfigsDropped:  st.dupConfigsDropped - base.dupConfigsDropped,
		gapsGlued:          st.gapsGlued - base.gapsGlued,
		gapMsRemoved:       st.gapMsRemoved - base.gapMsRemoved,
		dedupWindows:       st.dedupWindows - base.dedupWindows,
		dedupTags:          st.dedupTags - base.dedupTags,
		dedupMsDropped:     st.dedupMsDropped - base.dedupMsDropped,
		resets:             st.resets - base.resets,
		desyncs:            st.desyncs - base.desyncs,
		desyncBytesDrop:    st.desyncBytesDrop - base.desyncBytesDrop,
		keyframeWaitTags:   st.keyframeWaitTags - base.keyframeWaitTags,
		preConfigTags:      st.preConfigTags - base.preConfigTags,
		negativeTsTags:     st.negativeTsTags - base.negativeTsTags,
		jitterClamps:       st.jitterClamps - base.jitterClamps,
		tsSnapped:          st.tsSnapped - base.tsSnapped,
		ctsRepaired:        st.ctsRepaired - base.ctsRepaired,
		configChanges:      st.configChanges - base.configChanges,
		truncatedTailBytes: st.truncatedTailBytes - base.truncatedTailBytes,
	}
}

// Attrs returns the stats as alternating slog key/value pairs.
func (st Stats) Attrs() []any {
	return []any{
		"bytes_in", st.bytesIn,
		"bytes_out", st.bytesOut,
		"tags_in", st.tagsIn,
		"tags_out", st.tagsOut,
		"restarts_consumed", st.restartsConsumed,
		"scripts_dropped", st.scriptsDropped,
		"dup_configs_dropped", st.dupConfigsDropped,
		"gaps_glued", st.gapsGlued,
		"gap_ms_removed", st.gapMsRemoved,
		"dedup_windows", st.dedupWindows,
		"dedup_tags", st.dedupTags,
		"dedup_ms_dropped", st.dedupMsDropped,
		"resets", st.resets,
		"desyncs", st.desyncs,
		"desync_bytes_dropped", st.desyncBytesDrop,
		"keyframe_wait_tags", st.keyframeWaitTags,
		"pre_config_tags", st.preConfigTags,
		"negative_ts_tags", st.negativeTsTags,
		"jitter_clamps", st.jitterClamps,
		"ts_snapped", st.tsSnapped,
		"cts_repaired", st.ctsRepaired,
		"config_changes", st.configChanges,
		"truncated_tail_bytes", st.truncatedTailBytes,
	}
}
