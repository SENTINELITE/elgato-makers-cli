package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

const maxEntries = 100000

type box struct {
	kind       string
	start, end int64
}
type mp4Reader struct {
	r         io.ReaderAt
	count     int
	readBytes int64
}

func (p *mp4Reader) bytes(off int64, n int) ([]byte, error) {
	p.readBytes += int64(n)
	if p.readBytes > 8_000_000 {
		return nil, fmt.Errorf("MP4 metadata exceeds inspection budget")
	}
	if n < 0 || n > 1_000_000 {
		return nil, fmt.Errorf("metadata allocation exceeds limit")
	}
	b := make([]byte, n)
	_, e := p.r.ReadAt(b, off)
	return b, e
}
func (p *mp4Reader) boxes(start, end int64) ([]box, error) {
	var out []box
	for start < end {
		p.count++
		if p.count > maxEntries {
			return nil, fmt.Errorf("too many boxes")
		}
		if end-start < 8 {
			return nil, fmt.Errorf("truncated box header")
		}
		b, e := p.bytes(start, 8)
		if e != nil {
			return nil, e
		}
		size := uint64(binary.BigEndian.Uint32(b))
		header := int64(8)
		if size == 1 {
			if end-start < 16 {
				return nil, fmt.Errorf("truncated extended box")
			}
			b, e = p.bytes(start+8, 8)
			if e != nil {
				return nil, e
			}
			size = binary.BigEndian.Uint64(b)
			header = 16
		}
		if size == 0 {
			size = uint64(end - start)
		}
		if size < uint64(header) || size > uint64(end-start) {
			return nil, fmt.Errorf("invalid or truncated %q box", string(b[4:]))
		}
		kindBytes, e := p.bytes(start+4, 4)
		if e != nil {
			return nil, e
		}
		out = append(out, box{string(kindBytes), start + header, start + int64(size)})
		start += int64(size)
	}
	return out, nil
}
func unique(bs []box, kind string) (box, error) {
	var found box
	count := 0
	for _, b := range bs {
		if b.kind == kind {
			found = b
			count++
		}
	}
	if count != 1 {
		return box{}, fmt.Errorf("expected exactly one %s box", kind)
	}
	return found, nil
}
func (p *mp4Reader) payload(b box, min, max int) ([]byte, error) {
	n := b.end - b.start
	if n < int64(min) || n > int64(max) {
		return nil, fmt.Errorf("invalid %s metadata length", b.kind)
	}
	return p.bytes(b.start, int(n))
}
func inspectMP4(r io.ReaderAt, size int64) (int, int, error) {
	p := mp4Reader{r: r}
	top, e := p.boxes(0, size)
	if e != nil {
		return 0, 0, e
	}
	ft, e := unique(top, "ftyp")
	if e != nil {
		return 0, 0, e
	}
	fb, e := p.payload(ft, 8, 4096)
	if e != nil {
		return 0, 0, e
	}
	if len(fb)%4 != 0 {
		return 0, 0, fmt.Errorf("invalid file brands")
	}
	known := false
	for i := 0; i < len(fb); i += 4 {
		if i == 4 {
			continue
		}
		switch string(fb[i : i+4]) {
		case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "M4V ", "dash":
			known = true
		}
	}
	if !known {
		return 0, 0, fmt.Errorf("unsupported MP4 file brand")
	}
	var data []box
	for _, b := range top {
		switch b.kind {
		case "mdat":
			if b.end > b.start {
				data = append(data, b)
			}
		case "moof", "sinf", "pssh":
			return 0, 0, fmt.Errorf("fragmented or encrypted MP4 is unsupported")
		}
	}
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("missing nonempty media data")
	}
	moov, e := unique(top, "moov")
	if e != nil {
		return 0, 0, e
	}
	children, e := p.boxes(moov.start, moov.end)
	if e != nil {
		return 0, 0, e
	}
	if _, e = p.timing(children, "mvhd"); e != nil {
		return 0, 0, e
	}
	videos := 0
	w, h := 0, 0
	for _, b := range children {
		if b.kind == "mvex" || b.kind == "pssh" {
			return 0, 0, fmt.Errorf("fragmented or encrypted MP4 is unsupported")
		}
		if b.kind != "trak" {
			continue
		}
		tw, th, video, e := p.track(b, data)
		if e != nil {
			return 0, 0, e
		}
		if video {
			videos++
			w, h = tw, th
		}
	}
	if videos != 1 {
		return 0, 0, fmt.Errorf("expected exactly one video track")
	}
	return w, h, nil
}
func (p *mp4Reader) track(trak box, data []box) (int, int, bool, error) {
	tc, e := p.boxes(trak.start, trak.end)
	if e != nil {
		return 0, 0, false, e
	}
	mdia, e := unique(tc, "mdia")
	if e != nil {
		return 0, 0, false, e
	}
	mc, e := p.boxes(mdia.start, mdia.end)
	if e != nil {
		return 0, 0, false, e
	}
	hdlr, e := unique(mc, "hdlr")
	if e != nil {
		return 0, 0, false, e
	}
	hb, e := p.payload(hdlr, 24, 65536)
	if e != nil {
		return 0, 0, false, e
	}
	if hb[0] != 0 {
		return 0, 0, false, fmt.Errorf("unknown handler version")
	}
	if string(hb[8:12]) != "vide" {
		return 0, 0, false, nil
	}
	duration, e := p.timing(mc, "mdhd")
	if e != nil {
		return 0, 0, true, e
	}
	tkhd, e := unique(tc, "tkhd")
	if e != nil {
		return 0, 0, true, e
	}
	tb, e := p.payload(tkhd, 84, 96)
	if e != nil {
		return 0, 0, true, e
	}
	matrix := 40
	switch tb[0] {
	case 0:
		if len(tb) != 84 {
			return 0, 0, true, fmt.Errorf("invalid track header")
		}
	case 1:
		matrix = 52
		if len(tb) != 96 {
			return 0, 0, true, fmt.Errorf("invalid track header")
		}
	default:
		return 0, 0, true, fmt.Errorf("unknown track header version")
	}
	// Only an identity display transform is supported. Rotation, scaling, and
	// translation must not disguise dimensions that differ from the upload rule.
	expected := [9]uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	for i, v := range expected {
		if binary.BigEndian.Uint32(tb[matrix+i*4:]) != v {
			return 0, 0, true, fmt.Errorf("unsupported video rotation or display transform")
		}
	}
	wf, hf := binary.BigEndian.Uint32(tb[len(tb)-8:]), binary.BigEndian.Uint32(tb[len(tb)-4:])
	if wf&65535 != 0 || hf&65535 != 0 {
		return 0, 0, true, fmt.Errorf("fractional track dimensions are unsupported")
	}
	w, h := int(wf>>16), int(hf>>16)
	minf, e := unique(mc, "minf")
	if e != nil {
		return 0, 0, true, e
	}
	ic, e := p.boxes(minf.start, minf.end)
	if e != nil {
		return 0, 0, true, e
	}
	stbl, e := unique(ic, "stbl")
	if e != nil {
		return 0, 0, true, e
	}
	sc, e := p.boxes(stbl.start, stbl.end)
	if e != nil {
		return 0, 0, true, e
	}
	sd, e := unique(sc, "stsd")
	if e != nil {
		return 0, 0, true, e
	}
	head, e := p.bytes(sd.start, 8)
	if e != nil || sd.end-sd.start < 8 {
		return 0, 0, true, fmt.Errorf("truncated sample descriptions")
	}
	if binary.BigEndian.Uint32(head) != 0 || binary.BigEndian.Uint32(head[4:]) != 1 {
		return 0, 0, true, fmt.Errorf("unsupported sample description version or count")
	}
	entries, e := p.boxes(sd.start+8, sd.end)
	if e != nil {
		return 0, 0, true, e
	}
	if len(entries) != 1 {
		return 0, 0, true, fmt.Errorf("expected one video sample entry")
	}
	entry := entries[0]
	config := ""
	switch entry.kind {
	case "avc1", "avc3":
		config = "avcC"
	case "hvc1", "hev1":
		config = "hvcC"
	default:
		return 0, 0, true, fmt.Errorf("unsupported or encrypted video codec %q", entry.kind)
	}
	if entry.end-entry.start < 78 {
		return 0, 0, true, fmt.Errorf("truncated visual sample entry")
	}
	eb, e := p.bytes(entry.start, 78)
	if e != nil {
		return 0, 0, true, e
	}
	if binary.BigEndian.Uint16(eb[6:]) != 1 {
		return 0, 0, true, fmt.Errorf("external video data references are unsupported")
	}
	ew, eh := int(binary.BigEndian.Uint16(eb[24:])), int(binary.BigEndian.Uint16(eb[26:]))
	if ew == 0 || eh == 0 || ew != w || eh != h {
		return 0, 0, true, fmt.Errorf("sample entry and track dimensions disagree")
	}
	ec, e := p.boxes(entry.start+78, entry.end)
	if e != nil {
		return 0, 0, true, e
	}
	for _, b := range ec {
		switch b.kind {
		case config, "pasp", "btrt", "colr", "fiel":
		default:
			return 0, 0, true, fmt.Errorf("unsupported visual sample metadata %q", b.kind)
		}
		if b.kind == "sinf" {
			return 0, 0, true, fmt.Errorf("encrypted video is unsupported")
		}
		if b.kind == "pasp" {
			pb, e := p.payload(b, 8, 8)
			if e != nil {
				return 0, 0, true, e
			}
			if binary.BigEndian.Uint32(pb) == 0 || binary.BigEndian.Uint32(pb) != binary.BigEndian.Uint32(pb[4:]) {
				return 0, 0, true, fmt.Errorf("non-square video pixels are unsupported")
			}
		}
	}
	cfg, e := unique(ec, config)
	if e != nil {
		return 0, 0, true, e
	}
	if e = p.codec(cfg); e != nil {
		return 0, 0, true, e
	}
	if e = p.samples(sc, data, duration); e != nil {
		return 0, 0, true, e
	}
	return w, h, true, nil
}

// Check sample sizes, chunk mapping, and extents without reading media payloads.
func (p *mp4Reader) samples(bs []box, data []box, duration uint64) error {
	sz, e := unique(bs, "stsz")
	if e != nil {
		return e
	}
	b, e := p.payload(sz, 12, 12+maxEntries*4)
	if e != nil {
		return e
	}
	if binary.BigEndian.Uint32(b) != 0 {
		return fmt.Errorf("unknown sample size version")
	}
	fixed, n := binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:])
	if n == 0 || n > maxEntries {
		return fmt.Errorf("invalid or excessive video sample count")
	}
	if e = p.sampleTiming(bs, n, duration); e != nil {
		return e
	}
	sizes := make([]uint32, int(n))
	if fixed == 0 {
		if len(b) != 12+int(n)*4 {
			return fmt.Errorf("truncated sample size table")
		}
		for i := range sizes {
			sizes[i] = binary.BigEndian.Uint32(b[12+i*4:])
		}
	} else {
		if len(b) != 12 {
			return fmt.Errorf("invalid fixed sample size table")
		}
		for i := range sizes {
			sizes[i] = fixed
		}
	}
	co, e := unique(bs, "stco")
	wide := false
	if e != nil {
		co, e = unique(bs, "co64")
		wide = true
	}
	if e != nil {
		return e
	}
	for _, x := range bs {
		if (x.kind == "co64" && !wide) || (x.kind == "stco" && wide) {
			return fmt.Errorf("ambiguous chunk offsets")
		}
	}
	cb, e := p.payload(co, 8, 8+maxEntries*8)
	if e != nil {
		return e
	}
	count := binary.BigEndian.Uint32(cb[4:])
	step := 4
	if wide {
		step = 8
	}
	if binary.BigEndian.Uint32(cb) != 0 || count == 0 || count > maxEntries || len(cb) != 8+int(count)*step {
		return fmt.Errorf("invalid chunk offsets")
	}
	stsc, e := unique(bs, "stsc")
	if e != nil {
		return e
	}
	sb, e := p.payload(stsc, 8, 8+maxEntries*12)
	if e != nil {
		return e
	}
	maps := binary.BigEndian.Uint32(sb[4:])
	if binary.BigEndian.Uint32(sb) != 0 || maps == 0 || maps > maxEntries || len(sb) != 8+int(maps)*12 {
		return fmt.Errorf("invalid sample chunk map")
	}
	type mapping struct{ first, samples uint32 }
	ms := make([]mapping, int(maps))
	for i := range ms {
		off := 8 + i*12
		ms[i] = mapping{binary.BigEndian.Uint32(sb[off:]), binary.BigEndian.Uint32(sb[off+4:])}
		if ms[i].first == 0 || ms[i].first > count || ms[i].samples == 0 || binary.BigEndian.Uint32(sb[off+8:]) != 1 || (i == 0 && ms[i].first != 1) || (i > 0 && ms[i].first <= ms[i-1].first) {
			return fmt.Errorf("invalid sample chunk mapping")
		}
	}
	sample, mi := 0, 0
	for i := 0; i < int(count); i++ {
		if mi+1 < len(ms) && uint32(i+1) >= ms[mi+1].first {
			mi++
		}
		num := ms[mi].samples
		if uint64(sample)+uint64(num) > uint64(len(sizes)) {
			return fmt.Errorf("sample chunk exceeds sample table")
		}
		var length uint64
		for j := uint32(0); j < num; j++ {
			v := sizes[sample]
			if v == 0 {
				return fmt.Errorf("empty video sample")
			}
			length += uint64(v)
			sample++
		}
		off := uint64(binary.BigEndian.Uint32(cb[8+i*step:]))
		if wide {
			off = binary.BigEndian.Uint64(cb[8+i*step:])
		}
		idx := sort.Search(len(data), func(i int) bool { return uint64(data[i].end) > off })
		found := idx < len(data) && off >= uint64(data[idx].start) && length <= uint64(data[idx].end)-off
		if !found {
			return fmt.Errorf("video sample chunk lies outside media data")
		}
	}
	if sample != len(sizes) {
		return fmt.Errorf("unmapped video samples")
	}
	return nil
}

// Movie and media headers have fixed layouts for versions zero and one.
func (p *mp4Reader) timing(bs []box, kind string) (uint64, error) {
	b, e := unique(bs, kind)
	if e != nil {
		return 0, e
	}
	v, e := p.payload(b, 24, 112)
	if e != nil {
		return 0, e
	}
	base, want := 12, 24
	if kind == "mvhd" {
		want = 100
	}
	if v[0] == 1 {
		base = 20
		want += 12
	} else if v[0] != 0 {
		return 0, fmt.Errorf("unknown %s version", kind)
	}
	if len(v) != want {
		return 0, fmt.Errorf("invalid %s length", kind)
	}
	scale := binary.BigEndian.Uint32(v[base:])
	duration := uint64(binary.BigEndian.Uint32(v[base+4:]))
	if v[0] == 1 {
		duration = binary.BigEndian.Uint64(v[base+4:])
	}
	if scale == 0 || duration == 0 || duration == ^uint64(0) || (v[0] == 0 && duration == 0xffffffff) {
		return 0, fmt.Errorf("invalid %s timescale or duration", kind)
	}
	return duration, nil
}
func (p *mp4Reader) sampleTiming(bs []box, samples uint32, duration uint64) error {
	b, e := unique(bs, "stts")
	if e != nil {
		return e
	}
	v, e := p.payload(b, 8, 8+maxEntries*8)
	if e != nil {
		return e
	}
	n := binary.BigEndian.Uint32(v[4:])
	if binary.BigEndian.Uint32(v) != 0 || n == 0 || n > maxEntries || len(v) != 8+int(n)*8 {
		return fmt.Errorf("invalid decoding time table")
	}
	var count, ticks uint64
	for i := 0; i < int(n); i++ {
		c, d := uint64(binary.BigEndian.Uint32(v[8+i*8:])), uint64(binary.BigEndian.Uint32(v[12+i*8:]))
		if c == 0 || d == 0 || count+c > uint64(samples) {
			return fmt.Errorf("invalid decoding time entry")
		}
		count += c
		ticks += c * d
	}
	if count != uint64(samples) || ticks != duration {
		return fmt.Errorf("sample timing disagrees with video sample count or duration")
	}
	return nil
}
func (p *mp4Reader) codec(b box) error {
	v, e := p.payload(b, 7, 65536)
	if e != nil {
		return e
	}
	if v[0] != 1 {
		return fmt.Errorf("unsupported codec configuration version")
	}
	readNALs := func(pos, count int) (int, error) {
		for i := 0; i < count; i++ {
			if pos+2 > len(v) {
				return 0, fmt.Errorf("truncated codec NAL length")
			}
			n := int(binary.BigEndian.Uint16(v[pos:]))
			pos += 2
			if n == 0 || n > len(v)-pos {
				return 0, fmt.Errorf("truncated codec NAL data")
			}
			pos += n
		}
		return pos, nil
	}
	if b.kind == "avcC" {
		if v[4]&0xfc != 0xfc || v[4]&3 == 2 || v[5]&0xe0 != 0xe0 || v[5]&31 == 0 {
			return fmt.Errorf("invalid AVC configuration")
		}
		pos, e := readNALs(6, int(v[5]&31))
		if e != nil {
			return e
		}
		if pos >= len(v) || v[pos] == 0 {
			return fmt.Errorf("missing AVC PPS")
		}
		pos, e = readNALs(pos+1, int(v[pos]))
		if e != nil {
			return e
		}
		if pos < len(v) {
			switch v[1] {
			case 100, 110, 122, 144:
			default:
				return fmt.Errorf("unexpected AVC extension")
			}
			if len(v)-pos < 4 || v[pos]&0xfc != 0xfc || v[pos+1]&0xf8 != 0xf8 || v[pos+2]&0xf8 != 0xf8 {
				return fmt.Errorf("invalid AVC extension")
			}
			pos, e = readNALs(pos+4, int(v[pos+3]))
			if e != nil {
				return e
			}
		}
		if pos != len(v) {
			return fmt.Errorf("trailing AVC configuration data")
		}
		return nil
	}
	if len(v) < 23 {
		return fmt.Errorf("truncated HEVC configuration")
	}
	arrays := int(v[22])
	if arrays == 0 {
		return fmt.Errorf("missing HEVC parameter sets")
	}
	pos := 23
	for i := 0; i < arrays; i++ {
		if len(v)-pos < 3 {
			return fmt.Errorf("truncated HEVC array")
		}
		num := int(binary.BigEndian.Uint16(v[pos+1:]))
		if num == 0 {
			return fmt.Errorf("empty HEVC array")
		}
		pos, e = readNALs(pos+3, num)
		if e != nil {
			return e
		}
	}
	if pos != len(v) {
		return fmt.Errorf("trailing HEVC configuration data")
	}
	return nil
}
