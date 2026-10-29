package syncer

// Checksum1 is the rsync weak checksum. s1 and s2 are kept mod 2^16.
func Checksum1(buf []byte) uint32 {
	var s1, s2 uint32
	for _, c := range buf {
		s1 += uint32(c)
		s2 += s1
	}
	return (s1 & 0xffff) + (s2 << 16)
}

// RollChecksum slides a window of length n one byte forward.
// out is the byte leaving the window and in is the byte entering it.
func RollChecksum(sum uint32, n int, out, in byte) uint32 {
	s1 := (sum & 0xffff) + uint32(in) - uint32(out)
	s2 := (sum >> 16) + (s1 & 0xffff) - uint32(n)*uint32(out)
	return (s1 & 0xffff) + (s2 << 16)
}
