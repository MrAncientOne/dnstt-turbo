module www.bamsoftware.com/git/dnstt.git

go 1.26.0

toolchain go1.27.3

require (
	github.com/flynn/noise v1.0.0
	github.com/refraction-networking/utls v1.8.2
	github.com/xtaci/kcp-go/v5 v5.6.8
	github.com/xtaci/smux v1.5.24
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.60.0
)

require (
	github.com/andybalholm/brotli v1.0.6 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/klauspost/cpuid/v2 v2.2.6 // indirect
	github.com/klauspost/reedsolomon v1.12.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/templexxx/cpu v0.1.0 // indirect
	github.com/templexxx/xorsimd v0.4.2 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

// Preserve smux v2 compatibility with a minimal stream-registration fix.
replace github.com/xtaci/smux => ./third_party/smux
