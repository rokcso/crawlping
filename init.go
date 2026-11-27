package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// `crawlping init` writes the two files a working install needs, then prints
// what to do with them.
//
// Every first-run failure this project has had came from the configuration and
// not from the code: a container bound to loopback that no published port can
// reach, a key file in a subdirectory that narrows the key to that directory, a
// host written with a scheme, a key shorter than the spec allows, an
// indexnow_endpoint changed for no reason. Each of those is a value, so this
// writes the value instead of only documenting it - and comments it, because
// the file is what the next person reads.

const (
	// shapeDocker is the default: crawlping in a container, reached through a
	// published port. The address inside the container has to be the wildcard.
	shapeDocker = "docker"
	// shapeBinary is the same binary run straight from a shell on the host.
	shapeBinary = "binary"
	// shapeTunnel is crawlping sharing cloudflared's network namespace, so the
	// tunnel's own loopback is this process's loopback and no port is published.
	shapeTunnel = "tunnel"
)

const (
	initConfigName = "config.yaml"
	initConfigMode = 0o600
	initKeyMode    = 0o644
	initDirMode    = 0o755
	initListenPort = "8080"

	// 16 random bytes print as 32 hex characters, comfortably inside the spec's
	// 8-128 characters of [A-Za-z0-9-]. 32 print as 64, the shape
	// `openssl rand -hex 32` produces, which is what the README, the loader's
	// error message and the Worker example all assume.
	initKeyBytes   = 16
	initTokenBytes = 32
)

// randReader is crypto/rand.Reader. It is a variable only so a test can drive
// the two paths a generator that reads from it cannot otherwise reach: a fixed
// stream (to know the key file name in advance) and a failing one.
var randReader io.Reader = rand.Reader

// initOptions is the parsed flag set, kept apart from the flags themselves so
// the writer can be driven without os.Args.
type initOptions struct {
	dir   string
	host  string
	shape string
	force bool
}

// initResult is what was written, so the checklist names the same files and the
// same key the file on disk does.
type initResult struct {
	configPath string
	keyPath    string
	host       string
	key        string
	shape      string
}

// initShape is everything a deployment shape decides: the listen address, the
// comment that explains it, and the commands that start and check the service.
// They are one value because they are one decision - the listen address without
// its explanation is how the container-on-loopback mistake gets made - and
// keeping them together means the file and the printed checklist cannot
// disagree.
type initShape struct {
	listen        string
	listenComment []string
	start         []string
	startNote     []string
	check         []string
}

func cmdInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ExitOnError)

	dir := flags.String("dir", ".", "directory to write config.yaml and the key file into")
	host := flags.String("host", "", "the site to submit for, for example example.com")
	shape := flags.String("shape", shapeDocker, "how crawlping will run: docker, binary or tunnel")
	force := flags.Bool("force", false, "replace an existing config.yaml or key file")
	_ = flags.Parse(args)

	// A stray positional argument is a flag that lost its dash, and silently
	// ignoring it writes a config for the wrong host.
	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("init takes no arguments, got %q; -host is a flag: crawlping init -host example.com", rest[0])
	}

	return runInit(os.Stdout, initOptions{dir: *dir, host: *host, shape: *shape, force: *force})
}

func runInit(w io.Writer, opts initOptions) error {
	// The host is checked by the loader's own function, so `init` refuses the
	// same values with the same explanations the service would, rather than
	// growing a second opinion that the loader later contradicts.
	host, err := normalizeHost(opts.host)
	if err != nil {
		if opts.host == "" {
			return errors.New("-host is required: the site to submit for, for example -host example.com")
		}
		return fmt.Errorf(
			"-host %q is not a bare hostname (%v); use only the hostname, for example -host example.com",
			hint(opts.host, maxValueHint), err,
		)
	}

	dir := opts.dir
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve -dir %q: %w", dir, err)
	}

	configPath := filepath.Join(absDir, initConfigName)
	shape, err := initShapeFor(opts.shape, configPath)
	if err != nil {
		return err
	}

	key, err := randomHex(initKeyBytes)
	if err != nil {
		return err
	}
	token, err := randomHex(initTokenBytes)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(absDir, key+".txt")

	if err := os.MkdirAll(absDir, initDirMode); err != nil {
		return fmt.Errorf("create -dir %s: %w", absDir, err)
	}

	if !opts.force {
		for _, path := range []string{configPath, keyPath} {
			// Naming the file is the point: a refusal that only says "file
			// exists" hides which of the two would have been replaced.
			if err := refuseExisting(path); err != nil {
				return err
			}
		}
	}

	// The key file goes first. If the second write fails there is then an
	// unused key file and no config, rather than a config naming a key file
	// that was never written.
	if err := writeInitFile(keyPath, []byte(key+"\n"), initKeyMode, opts.force); err != nil {
		return err
	}
	if err := writeInitFile(configPath, []byte(renderConfig(host, key, token, shape)), initConfigMode, opts.force); err != nil {
		return err
	}

	// Write, then read back with the loader the service itself uses. The whole
	// contract of this command is that its output loads, so the check happens
	// here and not only in the tests: a template change cannot ship a file that
	// merely looks right.
	if _, err := loadConfig(configPath); err != nil {
		return fmt.Errorf("%s was written but does not load: %w", configPath, err)
	}

	printChecklist(w, initResult{
		configPath: configPath,
		keyPath:    keyPath,
		host:       host,
		key:        key,
		shape:      opts.shape,
	}, shape)

	return nil
}

// initShapeFor returns what one deployment shape demands, with the config path
// already substituted into its commands.
func initShapeFor(shape, configPath string) (initShape, error) {
	mount := fmt.Sprintf(`-v "%s:/etc/crawlping/config.yaml:ro"`, configPath)

	switch shape {
	case shapeDocker:
		return initShape{
			listen: "0.0.0.0:8080",
			listenComment: []string{
				"Docker forwards a published port to the container's eth0, never to",
				"the container's loopback, so this binds every interface *inside* the",
				"container. On 127.0.0.1:8080 here the container is unreachable from",
				"the host even with -p 8080:8080, and it still reports healthy, because",
				"its health probe dials the address the server is bound to.",
				"\"Reachable from this machine only\" is the host side of the mapping,",
				"-p 127.0.0.1:8080:8080, not this line.",
			},
			start: []string{
				"docker run -d --name crawlping --restart unless-stopped \\",
				"    -p 127.0.0.1:8080:" + initListenPort + " \\",
				"    " + mount + " \\",
				"    ghcr.io/rokcso/crawlping",
			},
			startNote: []string{
				"-p 127.0.0.1:8080:8080 keeps the API reachable from this machine only.",
				"Write -p 8080:8080 only if other machines really should reach it.",
			},
			check: []string{
				"docker run --rm \\",
				"    " + mount + " \\",
				"    ghcr.io/rokcso/crawlping check -config /etc/crawlping/config.yaml",
			},
		}, nil

	case shapeBinary:
		return initShape{
			listen: "127.0.0.1:" + initListenPort,
			listenComment: []string{
				"There is no container here and so no port forwarding to satisfy:",
				"loopback is where a process on this machine belongs. Only this machine",
				"can reach the API; put a reverse proxy in front of it for anything",
				"else. Binding 0.0.0.0 would publish it on every interface instead.",
			},
			start: []string{
				"crawlping serve -config " + configPath,
			},
			check: []string{
				"crawlping check -config " + configPath,
			},
		}, nil

	case shapeTunnel:
		return initShape{
			listen: "127.0.0.1:" + initListenPort,
			listenComment: []string{
				"crawlping joins cloudflared's network namespace, so cloudflared's",
				"http://127.0.0.1:" + initListenPort + " is this process's own loopback. No port is",
				"published at all. Only the inbound path goes through Cloudflare: the",
				"submissions still leave from this machine, which is the point.",
			},
			start: []string{
				"docker run -d --name crawlping --restart unless-stopped \\",
				"    --network container:cloudflared \\",
				"    " + mount + " \\",
				"    ghcr.io/rokcso/crawlping",
			},
			startNote: []string{
				"cloudflared's http://127.0.0.1:" + initListenPort + " is this container's loopback,",
				"so point the tunnel's public hostname at it and publish no port.",
			},
			check: []string{
				"docker run --rm \\",
				"    " + mount + " \\",
				"    ghcr.io/rokcso/crawlping check -config /etc/crawlping/config.yaml",
			},
		}, nil
	}

	return initShape{}, fmt.Errorf(
		"-shape %q is not one of %s, %s or %s",
		shape, shapeDocker, shapeBinary, shapeTunnel,
	)
}

// renderConfig is the file itself. Everything the shape decided is a value, and
// everything a value cannot express is a comment beside it, because the comment
// is what stops the next person from repeating the mistake.
func renderConfig(host, key, token string, shape initShape) string {
	var b []byte

	add := func(lines ...string) {
		for _, line := range lines {
			b = append(b, line...)
			b = append(b, '\n')
		}
	}

	add(
		"# Written by `crawlping init`. Mode 0600, because it holds a bearer token.",
		"#",
	)
	for _, line := range shape.listenComment {
		add("# " + line)
	}
	add(
		`listen: "`+shape.listen+`"`,
		"",
		"sites:",
		"  - host: "+host,
		"    # The key is also the file name. It has to be served at",
		"    #   https://"+host+"/"+key+".txt",
		"    # from the host root, because a key file inside a directory covers only",
		"    # the URLs under it: /catalog/<key>.txt submits /catalog/... and refuses",
		"    # everything else locally as out_of_key_scope.",
		`    indexnow_key: "`+key+`"`,
		"    # What a caller sends as \"Authorization: Bearer ...\". 64 hex characters,",
		"    # the shape `openssl rand -hex 32` produces.",
		`    token: "`+token+`"`,
		"",
		"# The IndexNow endpoint is deliberately absent, because the default is right",
		"# for almost everyone: the global endpoint shares one submission with every",
		"# participating engine. config.example.yaml lists the per-engine addresses",
		"# for the rare case of targeting a single engine instead.",
	)

	return string(b)
}

// printChecklist is the half of this command that is not a file: what to do
// with what was written, in the order it has to be done. The label column is
// the shape `crawlping check` already prints, so the two read as one tool.
func printChecklist(w io.Writer, res initResult, shape initShape) {
	keyFile := res.key + ".txt"

	fmt.Fprintf(w, "wrote %s (%04o) and %s (%04o) for %s, %s shape\n\n",
		res.configPath, initConfigMode, res.keyPath, initKeyMode, res.host, res.shape)

	fmt.Fprintf(w, "  key    serve %s at https://%s/%s\n", res.keyPath, res.host, keyFile)
	fmt.Fprintf(w, "         Cloudflare Worker, above the site's own routes:\n")
	fmt.Fprintf(w, "           if (new URL(request.url).pathname === %q) return new Response(%q, {headers: {\"content-type\": \"text/plain\"}});\n",
		"/"+keyFile, res.key+"\n")
	fmt.Fprintf(w, "         Static host, in whatever directory the site publishes (public/, dist/):\n")
	fmt.Fprintf(w, "           cp %s public/%s\n", res.keyPath, keyFile)
	fmt.Fprintf(w, "         Keep it at the root. A key file inside a directory covers only the\n")
	fmt.Fprintf(w, "         URLs under it, and everything else is refused as out_of_key_scope.\n")

	fmt.Fprintf(w, "  token  what the caller sends as \"Authorization: Bearer ...\", from the config:\n")
	fmt.Fprintf(w, "           grep token: %s\n", res.configPath)

	fmt.Fprintf(w, "  start  %s\n", shape.start[0])
	for _, line := range shape.start[1:] {
		fmt.Fprintf(w, "         %s\n", line)
	}
	for _, line := range shape.startNote {
		fmt.Fprintf(w, "         %s\n", line)
	}

	fmt.Fprintf(w, "  check  %s\n", shape.check[0])
	for _, line := range shape.check[1:] {
		fmt.Fprintf(w, "         %s\n", line)
	}
}

// randomHex returns n bytes of crypto/rand output as 2n lowercase hex
// characters: the shape both generated secrets need, and the one
// `openssl rand -hex n` produces.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		return "", fmt.Errorf("read %d random bytes: %w", n, err)
	}
	return hex.EncodeToString(buf), nil
}

// refuseExisting is the no-overwrite half of the promise, kept as its own
// message so it can name the file and the flag that overrides it.
func refuseExisting(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%s already exists; pass -force to replace it", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check %s: %w", path, err)
	}
}

// writeInitFile writes one generated file and sets its mode explicitly. The
// mode is not left to the open call: an overwritten file keeps the permissions
// it already had, so `-force` on a 0644 config.yaml would leave the token
// readable by everyone.
func writeInitFile(path string, body []byte, mode os.FileMode, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		// O_EXCL makes the refusal a fact rather than a check-then-write race:
		// a file created between refuseExisting and this call still wins.
		flags |= os.O_EXCL
	}

	file, err := os.OpenFile(path, flags, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; pass -force to replace it", path)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := file.Write(body); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set mode on %s: %w", path, err)
	}

	return nil
}
