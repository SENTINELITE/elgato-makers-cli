// Package mediatest supplies synthetic MP4 metadata for local contract tests.
// Its payload is not codec-decodable and must never be uploaded.
package mediatest

import "encoding/binary"

func Box(kind string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.BigEndian.PutUint32(b, uint32(n))
	copy(b[4:], kind)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}
func U32(values ...uint32) []byte {
	b := make([]byte, len(values)*4)
	for i, v := range values {
		binary.BigEndian.PutUint32(b[i*4:], v)
	}
	return b
}

// MP4 returns a synthetic 1920x1080 video container with one sample in mdat.
func MP4() []byte {
	ftyp := Box("ftyp", []byte("isom"), U32(0), []byte("mp42"))
	mdat := Box("mdat", []byte{1})
	tkhd := make([]byte, 84)
	tkhd[3] = 3
	for i, v := range []uint32{65536, 0, 0, 0, 65536, 0, 0, 0, 1 << 30} {
		binary.BigEndian.PutUint32(tkhd[40+i*4:], v)
	}
	binary.BigEndian.PutUint32(tkhd[76:], 1920<<16)
	binary.BigEndian.PutUint32(tkhd[80:], 1080<<16)
	hdlr := make([]byte, 24)
	copy(hdlr[8:], "vide")
	visual := make([]byte, 78)
	binary.BigEndian.PutUint16(visual[6:], 1)
	binary.BigEndian.PutUint16(visual[24:], 1920)
	binary.BigEndian.PutUint16(visual[26:], 1080)
	stsd := Box("stsd", U32(0, 1), Box("avc1", visual, Box("avcC", []byte{1, 66, 0, 40, 255, 225, 0, 1, 103, 1, 0, 1, 104})))
	stbl := Box("stbl", stsd, Box("stts", U32(0, 1, 1, 1000)), Box("stsz", U32(0, 1, 1)), Box("stco", U32(0, 1, uint32(len(ftyp)+8))), Box("stsc", U32(0, 1, 1, 1, 1)))
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], 1000)
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:], 1000)
	binary.BigEndian.PutUint32(mdhd[16:], 1000)
	moov := Box("moov", Box("mvhd", mvhd), Box("trak", Box("tkhd", tkhd), Box("mdia", Box("mdhd", mdhd), Box("hdlr", hdlr), Box("minf", stbl))))
	return append(append(ftyp, mdat...), moov...)
}
