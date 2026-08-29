package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const maxAuthorizationHeader = int64(64 << 10)

// readAuthorizationFD consumes the narrow descriptor contract emitted by
// `oauth with`: one HTTP Authorization header, never argv, environment, stdin,
// stdout, or the session log.
func readAuthorizationFD(fd int) (string, error) {
	if fd < 0 {
		return "", nil
	}
	if fd < 3 {
		return "", fmt.Errorf("-header-fd must be 3 or greater")
	}
	f := os.NewFile(uintptr(fd), "ask-authorization-fd-"+strconv.Itoa(fd))
	if f == nil {
		return "", fmt.Errorf("-header-fd %d is unavailable", fd)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAuthorizationHeader+1))
	if err != nil {
		return "", fmt.Errorf("reading -header-fd %d: %w", fd, err)
	}
	if int64(len(raw)) > maxAuthorizationHeader {
		return "", fmt.Errorf("authorization header exceeds %d bytes", maxAuthorizationHeader)
	}

	var authorization string
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			return "", fmt.Errorf("-header-fd line %d must be an Authorization header", lineNo+1)
		}
		if authorization != "" {
			return "", fmt.Errorf("-header-fd contains more than one Authorization header")
		}
		authorization = strings.TrimSpace(value)
		if authorization == "" || strings.ContainsAny(authorization, "\r\n\x00") {
			return "", fmt.Errorf("-header-fd contains an invalid Authorization header")
		}
	}
	if authorization == "" {
		return "", fmt.Errorf("-header-fd contains no Authorization header")
	}
	return authorization, nil
}
