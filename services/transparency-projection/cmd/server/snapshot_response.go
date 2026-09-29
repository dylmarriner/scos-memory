package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// gzipMinBytes is the threshold below which compressing costs more than it
// saves. Every real snapshot is far above it; an error body is far below.
const gzipMinBytes = 4096

// snapshotETag identifies the *content* of a snapshot: the views and the
// per-view error map, and nothing else.
//
// The "generated" timestamp is deliberately excluded. It changes on every
// request, and including it would make If-None-Match revalidation useless --
// the whole point is that an unchanged dashboard costs a few hundred bytes
// instead of a megabyte. The timestamp is still reported, as the
// X-SCOS-Generated response header, so the 304 tells the client when the
// content it already holds was assembled.
func snapshotETag(views map[string]json.RawMessage, unavailable map[string]string) string {
	h := sha256.New()
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(views[name])
		h.Write([]byte{1})
	}
	errNames := make([]string, 0, len(unavailable))
	for name := range unavailable {
		errNames = append(errNames, name)
	}
	sort.Strings(errNames)
	for _, name := range errNames {
		h.Write([]byte(name))
		h.Write([]byte{2})
		h.Write([]byte(unavailable[name]))
		h.Write([]byte{1})
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:20]) + `"`
}

// etagMatches applies the If-None-Match comparison rules from RFC 9110
// §13.1.2 to the extent a single opaque validator needs: "*" matches
// anything, a weak prefix is ignored, and a comma-separated list matches if
// any member does.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client offered gzip in Accept-Encoding,
// honouring parameters ("gzip;q=0.5") and the wildcard form.
func acceptsGzip(r *http.Request) bool {
	wildcard := false
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		token := strings.TrimSpace(part)
		q := 1.0
		if i := strings.IndexByte(token, ';'); i != -1 {
			if params := token[i+1:]; strings.Contains(params, "q=") {
				for _, p := range strings.Split(params, ";") {
					p = strings.TrimSpace(p)
					if strings.HasPrefix(p, "q=") {
						if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
							q = v
						}
					}
				}
			}
			token = strings.TrimSpace(token[:i])
		}
		if strings.EqualFold(token, "*") {
			wildcard = true
			continue
		}
		if strings.EqualFold(token, "gzip") && q > 0 {
			return true
		}
	}
	return wildcard
}

// writeSnapshot writes the assembled snapshot, compressing it when the
// client asked for gzip. The body is close to a megabyte uncompressed
// (contradictions alone is ~700KB), so this is the difference between a poll
// costing a megabyte and costing roughly seventy kilobytes.
//
// Compression is applied only when every byte is available: the body is
// marshalled before this is called, so a mid-write failure can only mean a
// broken connection, not a half-streamed encoder.
func writeSnapshot(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vary", "Accept-Encoding")
	if len(body) >= gzipMinBytes && acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(status)
		zw := gzip.NewWriter(w)
		if _, err := zw.Write(body); err != nil {
			log.Printf("snapshot: gzip write failed: %v", err)
			return
		}
		if err := zw.Close(); err != nil {
			log.Printf("snapshot: gzip close failed: %v", err)
		}
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("snapshot: write failed: %v", err)
	}
}
