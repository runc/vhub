package douyin

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// xBogusCharacter is the custom alphabet of Douyin's X-Bogus signature
// (ported from utils/xbogus.py of the reference implementation).
const xBogusCharacter = "Dkdpgh4ZKsQB80/Mfvw36XI1R25-WUAlEi7NLboqYTOPuzmFjJnryx9HVGcaStCe="

var xBogusUAKey = []byte{0x00, 0x01, 0x0c}

// xBogusSigner computes the X-Bogus query parameter for a URL. It mirrors
// Douyin's front-end JS signer: MD5 digests of the URL and the RC4-encrypted
// user agent are mixed with a timestamp and RC4-encrypted again, then
// encoded in the custom alphabet.
type xBogusSigner struct {
	userAgent string
}

func newXBogusSigner(userAgent string) *xBogusSigner {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36"
	}
	return &xBogusSigner{userAgent: userAgent}
}

// hexToBytes mirrors _md5_str_to_array: hex strings become their 16 raw
// bytes; longer strings become their byte values verbatim.
func hexToBytes(s string) []byte {
	if len(s) > 32 {
		return []byte(s)
	}
	out, err := hex.DecodeString(s)
	if err != nil {
		return []byte(s)
	}
	return out
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// md5Digest mirrors _md5: for a string input it hashes hexToBytes(s),
// for a byte slice it hashes the bytes directly.
func md5DigestHex(data []byte) []byte {
	sum := md5.Sum(data)
	return sum[:]
}

// urlMD5 mirrors _md5_encrypt: md5(md5_raw(url)).
func urlMD5(urlPath string) []byte {
	return md5DigestHex(hexToBytes(md5Hex(hexToBytes(urlPath))))
}

func rc4(key, data []byte) []byte {
	s := make([]int, 256)
	for i := range s {
		s[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + s[i] + int(key[i%len(key)])) % 256
		s[i], s[j] = s[j], s[i]
	}
	out := make([]byte, 0, len(data))
	i, jj := 0, 0
	for _, b := range data {
		i = (i + 1) % 256
		jj = (jj + s[i]) % 256
		s[i], s[jj] = s[jj], s[i]
		out = append(out, b^byte(s[(s[i]+s[jj])%256]))
	}
	return out
}

func xbCalculation(a1, a2, a3 int) string {
	x3 := ((a1 & 255) << 16) | ((a2 & 255) << 8) | (a3 & 255)
	return string([]byte{
		xBogusCharacter[(x3&16515072)>>18],
		xBogusCharacter[(x3&258048)>>12],
		xBogusCharacter[(x3&4032)>>6],
		xBogusCharacter[x3&63],
	})
}

// build returns (signedURL, xbogus, userAgent).
func (s *xBogusSigner) build(rawurl string) (string, string, string) {
	uaEncrypted := rc4(xBogusUAKey, []byte(s.userAgent))
	uaMD5 := hexToBytes(md5Hex([]byte(base64.StdEncoding.EncodeToString(uaEncrypted))))

	emptyMD5 := md5DigestHex(hexToBytes("d41d8cd98f00b204e9800998ecf8427e"))
	urlHash := urlMD5(rawurl)

	timer := int(time.Now().Unix())
	ct := 536919696

	// The Python implementation interleaves then de-interleaves the array,
	// which is an identity shuffle except that the float 0.00390625 becomes
	// int(...) = 0. The payload therefore equals newArray with index 1 = 0.
	payload := []int{
		64, 0, 1, 12,
		int(urlHash[14]), int(urlHash[15]),
		int(emptyMD5[14]), int(emptyMD5[15]),
		int(uaMD5[14]), int(uaMD5[15]),
		timer >> 24 & 255, timer >> 16 & 255, timer >> 8 & 255, timer & 255,
		ct >> 24 & 255, ct >> 16 & 255, ct >> 8 & 255, ct & 255,
	}
	xorResult := payload[0]
	for _, v := range payload[1:] {
		xorResult ^= v
	}
	payload = append(payload, xorResult)

	payloadBytes := make([]byte, len(payload))
	for i, v := range payload {
		payloadBytes[i] = byte(v)
	}
	garbled := append([]byte{2, 255}, rc4([]byte{0xFF}, payloadBytes)...)

	xb := ""
	for i := 0; i+2 < len(garbled); i += 3 {
		xb += xbCalculation(int(garbled[i]), int(garbled[i+1]), int(garbled[i+2]))
	}
	return rawurl + "&X-Bogus=" + xb, xb, s.userAgent
}
