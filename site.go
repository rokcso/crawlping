package main

// `crawlping site` is the answer to the one operation an operator repeats:
// adding a host. Doing it by hand is six steps with two traps - generate two
// secrets, edit YAML, fix the file mode, publish the key, remember to run
// `check` before restarting - and the traps are the kind a person hits at the
// end of a long deploy rather than the kind a careful person avoids.
//
// The command edits the file it was pointed at, then reads the result back with
// the loader the service itself uses. Nothing here holds a second opinion about
// what a valid config is: if the edited file would not start the service, the
// command refuses to write it and says which line is wrong.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func cmdSite(args []string) error {
	if len(args) == 0 {
		return errors.New("site needs a subcommand: add, rm or list")
	}

	// The subcommand is the first argument and the rest are its flags, so
	// `site add -host x` and `site -config c add -host x` cannot both be true.
	// Only the first form is accepted, which keeps every flag's meaning local to
	// the subcommand that reads it.
	switch args[0] {
	case "add":
		return cmdSiteAdd(args[1:])
	case "rm":
		return cmdSiteRemove(args[1:])
	case "list", "ls":
		return cmdSiteList(args[1:])
	}

	return fmt.Errorf("site %q is not a subcommand; use add, rm or list", args[0])
}

type siteAddOptions struct {
	configPath string
	host       string
	key        string
	token      string
	keyDir     string
}

func cmdSiteAdd(args []string) error {
	flags := flag.NewFlagSet("site add", flag.ExitOnError)

	var opts siteAddOptions
	flags.StringVar(&opts.configPath, "config", defaultConfigPath(), "path to config.yaml")
	flags.StringVar(&opts.host, "host", "", "the site to add, for example example.com")
	flags.StringVar(&opts.key, "key", "", "indexnow key; generated when empty, set it to reuse a key file already deployed")
	flags.StringVar(&opts.token, "token", "", "bearer token; generated when empty")
	flags.StringVar(&opts.keyDir, "keydir", "", "directory for the generated <key>.txt; defaults to the config's directory")
	_ = flags.Parse(args)

	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("site add takes no arguments, got %q; -host is a flag: crawlping site add -host example.com", rest[0])
	}

	return runSiteAdd(os.Stdout, opts)
}

func runSiteAdd(w io.Writer, opts siteAddOptions) error {
	host, err := normalizeHost(opts.host)
	if err != nil {
		if opts.host == "" {
			return errors.New("-host is required: the site to add, for example -host example.com")
		}
		return fmt.Errorf(
			"-host %q is not a bare hostname (%v); use only the hostname, for example -host example.com",
			hint(opts.host, maxValueHint), err,
		)
	}

	key := opts.key
	if key == "" {
		if key, err = randomHex(initKeyBytes); err != nil {
			return err
		}
	}
	token := opts.token
	if token == "" {
		if token, err = randomHex(initTokenBytes); err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(opts.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist yet; run crawlping init -host %s first", opts.configPath, host)
		}
		return fmt.Errorf("read config: %w", err)
	}

	// The file as it stands has to be valid first. Editing a config the service
	// cannot load would hide the original problem behind the new one.
	current, err := parseConfig(raw)
	if err != nil {
		return fmt.Errorf("%s does not load, so nothing was written: %w", opts.configPath, err)
	}
	for _, site := range current.Sites {
		if site.Host == host {
			return fmt.Errorf("%s is already in %s; use crawlping site rm -host %s first to replace it", host, opts.configPath, host)
		}
	}

	edited, err := appendSite(raw, Site{Host: host, IndexNowKey: key, Token: token})
	if err != nil {
		return fmt.Errorf("edit %s: %w", opts.configPath, err)
	}

	// Read the bytes back through the loader before they reach the disk. This is
	// the same promise `init` makes: a command whose whole output is a file has
	// to prove the file loads, not merely look right.
	next, err := parseConfig(edited)
	if err != nil {
		return fmt.Errorf("the edited config does not load, so nothing was written: %w", err)
	}

	keyDir := opts.keyDir
	if keyDir == "" {
		keyDir = filepath.Dir(opts.configPath)
	}
	keyPath := filepath.Join(keyDir, key+".txt")

	// The key file goes first, for the reason init writes it first: a failure
	// here leaves an unused key file, while the other order would leave a config
	// naming a key file that does not exist.
	if err := writeInitFile(keyPath, []byte(key+"\n"), initKeyMode, false); err != nil {
		return err
	}

	// Atomic, and with the mode the file already had: an editor or a `>` can
	// leave a config readable by everyone, and the token lives in it.
	if err := writeConfigAtomically(opts.configPath, edited); err != nil {
		return err
	}

	site := next.Sites[len(next.Sites)-1]
	printSiteAdded(w, opts.configPath, keyPath, site, len(next.Sites), opts.key != "")

	return nil
}

// appendSite adds one entry to the sites list of an existing document.
//
// It edits the YAML node tree rather than re-rendering from a parsed Config, so
// every comment, key order and blank line the operator wrote survives the edit.
// A config file is read by a person more often than by the loader, and a command
// that silently reformats it is a command people stop running.
func appendSite(raw []byte, site Site) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("the file is not a YAML document")
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the top level is not a mapping")
	}

	entry := siteNode(site)

	var sites *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "sites" {
			sites = root.Content[i+1]
			break
		}
	}

	switch {
	case sites == nil:
		// A config with no sites does not load, so this is unreachable after the
		// validation above - kept because the function should not depend on a
		// check made somewhere else.
		sites = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sites"},
			sites,
		)
	case sites.Kind != yaml.SequenceNode:
		return nil, errors.New("the sites entry is not a list")
	}

	sites.Content = append(sites.Content, entry)

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	// Two spaces, which is what the example config and every other file in this
	// repository use. The encoder's own default is four.
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func siteNode(site Site) *yaml.Node {
	field := func(name, value string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name, Style: 0}
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: site.IndexNowKey}
	// The same note the generated config carries, in the one place a second site
	// is written: the key is also the name of the file that has to be served.
	keyNode.LineComment = " also the name of the key file served at https://" + site.Host + "/" + site.IndexNowKey + ".txt"

	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			field("host", "host"), {Kind: yaml.ScalarNode, Tag: "!!str", Value: site.Host},
			field("indexnow_key", "indexnow_key"), keyNode,
			field("token", "token"), {Kind: yaml.ScalarNode, Tag: "!!str", Value: site.Token},
		},
	}
}

// writeConfigAtomically replaces the file through a temporary one in the same
// directory, so a failure halfway through leaves the old file intact rather than
// a truncated config the service would refuse to start on. The temporary file
// gets the original's mode, not the umask's: the token is in there.
func writeConfigAtomically(path string, body []byte) error {
	mode := os.FileMode(initConfigMode)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tempPath := temp.Name()

	defer func() {
		// Best effort: after a successful rename this path no longer exists, and
		// a stray temporary file is not worth failing a completed command over.
		_ = os.Remove(tempPath)
	}()

	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return fmt.Errorf("set mode on %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	return nil
}

func cmdSiteRemove(args []string) error {
	flags := flag.NewFlagSet("site rm", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "path to config.yaml")
	host := flags.String("host", "", "the site to remove")
	_ = flags.Parse(args)

	if *host == "" {
		return errors.New("-host is required: the site to remove")
	}

	return runSiteRemove(os.Stdout, *configPath, *host)
}

func runSiteRemove(w io.Writer, configPath, host string) error {
	normalized, err := normalizeHost(host)
	if err != nil {
		return fmt.Errorf("-host %q is not a bare hostname (%v)", hint(host, maxValueHint), err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	current, err := parseConfig(raw)
	if err != nil {
		return fmt.Errorf("%s does not load, so nothing was removed: %w", configPath, err)
	}
	if len(current.Sites) == 1 {
		// A config with no sites does not load, so removing the last one would
		// write a file the service refuses to start on. Refusing here is the
		// difference between a guard and a mistake.
		return fmt.Errorf("%s is the only site in %s; a config needs at least one, so it was not removed", normalized, configPath)
	}

	edited, removed, err := removeSite(raw, normalized)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("%s is not in %s", normalized, configPath)
	}

	next, err := parseConfig(edited)
	if err != nil {
		return fmt.Errorf("the edited config does not load, so nothing was written: %w", err)
	}
	if err := writeConfigAtomically(configPath, edited); err != nil {
		return err
	}

	fmt.Fprintf(w, "removed %s from %s, %d left\n", normalized, configPath, len(next.Sites))
	// The key file is left alone on purpose: the site may still be serving it,
	// and a file this command created is not one it should quietly delete.
	fmt.Fprintf(w, "\n  the key file was left in place; delete it from the site when the site is gone\n")
	fmt.Fprintf(w, "  reload with: docker kill -s HUP crawlping\n")

	return nil
}

func removeSite(raw []byte, host string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, errors.New("the file is not a YAML document")
	}

	root := doc.Content[0]

	var sites *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "sites" {
			sites = root.Content[i+1]
			break
		}
	}
	if sites == nil || sites.Kind != yaml.SequenceNode {
		return nil, false, errors.New("the config has no sites list")
	}

	kept := make([]*yaml.Node, 0, len(sites.Content))
	removed := false
	for _, entry := range sites.Content {
		if entry.Kind == yaml.MappingNode && mappingValue(entry, "host") == host {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return nil, false, nil
	}

	sites.Content = kept

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := encoder.Close(); err != nil {
		return nil, false, err
	}

	return buf.Bytes(), true, nil
}

func mappingValue(mapping *yaml.Node, key string) string {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1].Value
		}
	}
	return ""
}

func cmdSiteList(args []string) error {
	flags := flag.NewFlagSet("site list", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "path to config.yaml")
	_ = flags.Parse(args)

	return runSiteList(os.Stdout, *configPath)
}

func runSiteList(w io.Writer, configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	hosts := make([]string, 0, len(cfg.Sites))
	for _, site := range cfg.Sites {
		hosts = append(hosts, site.Host)
	}
	sort.Strings(hosts)

	width := 4
	for _, host := range hosts {
		if len(host) > width {
			width = len(host)
		}
	}

	fmt.Fprintf(w, "%-*s  %-22s  %-6s  %s\n", width, "HOST", "KEY (public)", "TOKEN", "KEY FILE")
	for _, host := range hosts {
		for _, site := range cfg.Sites {
			if site.Host != host {
				continue
			}
			// The key is public by design - it is served at /<key>.txt - so it
			// is printed in full. The token is the only secret in this table and
			// is shown by its last four characters, enough to tell two entries
			// apart and not enough to use one.
			fmt.Fprintf(w, "%-*s  %-22s  %-6s  %s\n",
				width, site.Host, site.IndexNowKey, maskedToken(site.Token), site.KeyLocation)
		}
	}

	return nil
}

func maskedToken(token string) string {
	const keep = 4
	if len(token) <= keep {
		return strings.Repeat("•", len(token))
	}
	return strings.Repeat("•", 3) + token[len(token)-keep:]
}

func printSiteAdded(w io.Writer, configPath, keyPath string, site Site, total int, reusedKey bool) {
	keyFile := site.IndexNowKey + ".txt"

	fmt.Fprintf(w, "added %s to %s, %d sites\n\n", site.Host, configPath, total)

	if reusedKey {
		fmt.Fprintf(w, "  key    reusing %s, already published at %s\n", site.IndexNowKey, site.KeyLocation)
	} else {
		fmt.Fprintf(w, "  key    wrote %s\n", keyPath)
		fmt.Fprintf(w, "         serve it at %s\n", site.KeyLocation)
		fmt.Fprintf(w, "           Cloudflare Worker, above the site's own routes:\n")
		fmt.Fprintf(w, "             if (new URL(request.url).pathname === %q) return new Response(%q, {headers: {\"content-type\": \"text/plain\"}});\n",
			"/"+keyFile, site.IndexNowKey+"\n")
		fmt.Fprintf(w, "           Static host, in whatever directory the site publishes:\n")
		fmt.Fprintf(w, "             cp %s public/%s\n", keyPath, keyFile)
	}

	fmt.Fprintf(w, "\n  token  what this site's caller sends as \"Authorization: Bearer ...\",\n")
	fmt.Fprintf(w, "         stored in %s; `crawlping site list` shows its last four characters\n", configPath)

	fmt.Fprintf(w, "\n  apply  docker kill -s HUP crawlping     (no restart, no dropped request)\n")
	fmt.Fprintf(w, "  check  crawlping check -config %s\n", configPath)
	fmt.Fprintf(w, "\n  Nothing is submitted for %s until its caller does. The key file above only\n", site.Host)
	fmt.Fprintf(w, "  has to be live before the first submission, not before this command.\n")
}
