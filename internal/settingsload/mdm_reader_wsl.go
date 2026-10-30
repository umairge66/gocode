package settingsload

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// errRegOutputTooLarge marks reg.exe output past the 2 MiB buffer.
var errRegOutputTooLarge = errors.New("reg.exe output exceeds the buffer")

// cappedBuffer collects at most max bytes and fails the write beyond that
// .
type cappedBuffer struct {
	buf bytes.Buffer
	max int
	hit bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.hit = true
		return 0, errRegOutputTooLarge
	}
	return c.buf.Write(p)
}

// regQueryResult is one `reg query` outcome.
type regQueryResult struct {
	stdout   string
	ok       bool // exited 0
	oversize bool // stdout exceeded the cap
}

// runRegQuery runs `<regExe> query <key> /v Settings` with the configured
// timeout and output cap. It is a local process read only.
func runRegQuery(regExe, key string) regQueryResult {
	ctx, cancel := context.WithTimeout(context.Background(), registryQueryTimeoutMs*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, regExe, "query", key, "/v", RegistryValueName)
	out := &cappedBuffer{max: registryMaxBytes}
	cmd.Stdout = out
	err := cmd.Run()
	if out.hit {
		return regQueryResult{oversize: true}
	}
	if err != nil {
		return regQueryResult{}
	}
	return regQueryResult{stdout: out.buf.String(), ok: true}
}

// readoutFromRegQueries builds the registry half of the readout from the
// HKLM / HKCU `reg query` results, extracting the "Settings" value from each
// .
// A successful query whose output carries no Settings line yields an empty
// document, which the parser reports as not a JSON object.
func readoutFromRegQueries(hklm, hkcu regQueryResult) OSPolicyReadout {
	var r OSPolicyReadout
	if hklm.ok {
		v, _ := ExtractRegistryValue(hklm.stdout)
		r.HKLM = &v
	} else if hklm.oversize {
		r.HKLMUnreadReason = registryUnreadOversize
	}
	if hkcu.ok {
		v, _ := ExtractRegistryValue(hkcu.stdout)
		r.HKCU = &v
	}
	return r
}

// readWSLOSPolicy queries the Windows policy keys through reg.exe from WSL
// .
func readWSLOSPolicy() OSPolicyReadout {
	type res struct {
		i int
		r regQueryResult
	}
	ch := make(chan res, 2)
	for i, key := range []string{RegistryKeyHKLM, RegistryKeyHKCU} {
		go func(i int, key string) { ch <- res{i, runRegQuery(WSLRegExe, key)} }(i, key)
	}
	var out [2]regQueryResult
	for range 2 {
		x := <-ch
		out[x.i] = x.r
	}
	r := readoutFromRegQueries(out[0], out[1])
	r.IsWSL = true
	return r
}
