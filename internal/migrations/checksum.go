package migrations

import (
	"hash/crc32"
	"unicode/utf8"
)

// Checksum returns the value Flyway stores in its schema history table for a
// SQL file. Flyway reads the file line by line (Java's readLine, which splits
// on \n, \r and \r\n), drops a leading byte order mark, and feeds each line's
// UTF-8 bytes without its terminator into a CRC32, then stores the result as a
// signed 32-bit integer. So the checksum ignores line-ending style but not
// whitespace inside lines.
//
// The test vectors in checksum_test.go come from Flyway 13.10.3.
func Checksum(content []byte) int32 {
	content = normalizeUTF8(content)
	if len(content) >= 3 && content[0] == 0xEF && content[1] == 0xBB && content[2] == 0xBF {
		content = content[3:]
	}
	crc := crc32.NewIEEE()
	start := 0
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\n':
			crc.Write(content[start:i])
			start = i + 1
		case '\r':
			crc.Write(content[start:i])
			if i+1 < len(content) && content[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	crc.Write(content[start:])
	return int32(crc.Sum32())
}

// normalizeUTF8 mirrors Java decoding a file as UTF-8 with replacement: each
// invalid byte becomes U+FFFD. Valid UTF-8 comes back unchanged.
func normalizeUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	out := make([]byte, 0, len(b)+16)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
		} else {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}
