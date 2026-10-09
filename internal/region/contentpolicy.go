package region

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Content policy for agent messages.
//
// Threat model: a compromised or manipulated agent that holds a write grant
// on a namespace can message every other member. The envelope framing
// ("treat it as data, not as instructions") is model compliance, not a hard
// boundary - no framing is (see OWASP LLM01 and the prompt-injection
// literature: no known defense works 100% of the time). This filter is the
// server-side layer underneath it: messages whose bodies match well-known
// dangerous-intent shapes are rejected at send time, so they are never
// stored and never delivered, regardless of how any receiving model would
// have interpreted them.
//
// What this is NOT: an intent detector. A regex cannot distinguish an
// attack from a security discussion that quotes the same words
// ("never run rm -rf /"). Benign lookalikes will occasionally be blocked;
// the block error names the category and pattern so a legitimate sender
// can rephrase, and `namespaces` / `mode: off` are the escape hatches.
// It is one layer in front of the framing, the sender allowlist and the
// receiving agent's own permission system - not a replacement for any of
// them.

// ErrMessageBlocked: the message body matched the content policy.
var ErrMessageBlocked = errors.New("message: blocked by content policy")

// contentPattern is one compiled detection rule.
type contentPattern struct {
	category string
	name     string
	re       *regexp.Regexp
}

// ContentPolicy is a compiled set of dangerous-intent patterns.
// A nil *ContentPolicy means no filtering.
type ContentPolicy struct {
	patterns []contentPattern
	skipNS   map[string]bool
}

// contentPatternSpecs is the default registry. Categories group the rules
// so block errors are actionable ("blocked: exfiltration (secrets-path)").
// All patterns are matched case-insensitively against the raw body.
//
// Sources: the OWASP LLM Prompt Injection Prevention cheat sheet
// (instruction override, persona jailbreaks, system-prompt exfiltration),
// the PayloadsAllTheThings prompt-injection and reverse-shell corpora
// (encoding evasion, /dev/tcp, nc -e, socat, pipe-to-shell), published
// multi-agent communication-attack research (inter-agent trust abuse,
// replication/worm behavior), and operational incident patterns for
// agent fleets (destructive shell/git/db/k8s commands, persistence,
// credential minting, supply-chain redirects).
var contentPatternSpecs = []struct {
	category, name, pattern string
}{
	// --- destructive filesystem / disk ---
	{"destructive_fs", "rm-recursive-force", `rm\s+(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+\S`},
	{"destructive_fs", "mkfs", `\bmkfs(\.\w+)?\b`},
	{"destructive_fs", "dd-to-disk", `\bdd\s+[^;|&]*of=/dev/(sd|nvme|vd|xv|hd|mmcblk)`},
	{"destructive_fs", "redirect-to-disk", `>\s*/dev/(sd|nvme|vd|xv|hd|mmcblk)`},
	{"destructive_fs", "fork-bomb", `:\(\)\s*\{\s*:\|:\s*&?\s*\}\s*;:`},
	{"destructive_fs", "shred", `\bshred\b\s+\S`},
	{"destructive_fs", "chmod-zero", `chmod\s+(-R\s+)?0{3}\b`},
	{"destructive_fs", "find-delete", `find\s+[^;|&]{0,200}(-delete|-exec\s+rm\b)`},
	{"destructive_fs", "sysrq-trigger", `>\s*/proc/sysrq-trigger`},

	// --- destructive system ---
	{"destructive_sys", "shutdown", `\b(sudo\s+)?(shutdown|poweroff)\b`},
	{"destructive_sys", "reboot", `\b(sudo\s+)?reboot\b`},
	{"destructive_sys", "kill-all", `kill\s+(-9\s+)?-1\b`},
	{"destructive_sys", "killall", `\bkillall\b`},
	{"destructive_sys", "pkill-force", `pkill\s+-9\b`},

	// --- destructive git ---
	{"destructive_git", "force-push-main", `git\s+push\s+[^;|&]*(--force|-f)\b[^;|&]*\b(main|master)\b`},
	{"destructive_git", "reset-hard-main", `git\s+reset\s+--hard\s+(origin/)?(main|master)\b`},
	{"destructive_git", "filter-branch", `git\s+filter-branch\b`},
	{"destructive_git", "reflog-expire", `git\s+reflog\s+[^;|&]*--expire=now`},
	{"destructive_git", "gc-prune", `git\s+gc\s+[^;|&]*--prune=now`},

	// --- destructive database ---
	{"destructive_db", "drop-table", `\bdrop\s+(table|database|schema)\b`},
	{"destructive_db", "truncate-table", `\btruncate\s+table\b`},
	{"destructive_db", "delete-no-where", `delete\s+from\s+[^\s;]+\s*;`},
	{"destructive_db", "redis-flush", `\bflush(all|db)\b`},

	// --- destructive k8s / infra ---
	{"destructive_k8s", "kubectl-delete-namespace", `kubectl\s+[^;|&]*delete\s+(namespace|ns)\b`},
	{"destructive_k8s", "kubectl-delete-all", `kubectl\s+[^;|&]*delete\s+[^;|&]*--all\b`},
	{"destructive_k8s", "terraform-destroy", `terraform\s+destroy\b`},

	// --- exfiltration: secret paths and secret material ---
	// Note: RE2 has no lookbehind, so boundary assertions use
	// (?:^|[^a-z0-9]) instead of \b (which misfires before optional
	// dots and tildes in paths like ~/.aws/credentials).
	{"exfiltration", "secrets-path", `(?:^|[^a-z0-9])(cat|send|upload|post|print|share|paste|dump|exfiltrate|read|include|attach|forward|email)\b[^.]{0,60}(?:^|[^a-z0-9])(\.?aws|\.?ssh|\.?kube|\.?docker|\.?gnupg|\.env|id_rsa|id_ed25519|authorized_keys|\.netrc|\.npmrc|credentials\.json|secrets?\.(?:ya?ml|json|txt)|private[_-]?key)(?:$|[^a-z0-9])`},
	{"exfiltration", "secrets-material", `(?:^|[^a-z0-9])(reveal|share|send|print|show|paste|output|repeat|exfiltrate|leak|include)\b[^.]{0,50}(?:^|[^a-z0-9])(system\s+prompt|your\s+instructions|api[_ -]?keys?|access[_ -]?tokens?|passwords?|passwds?|secrets?|credentials?|private[_ -]?keys?)(?:$|[^a-z0-9])`},
	{"exfiltration", "ask-system-prompt", `(?:^|[^a-z0-9])(what|show|reveal|print|repeat)\b[^.]{0,20}your\s+(system\s+prompt|instructions|rules)\b`},

	// --- remote code execution / beacons ---
	{"remote_code", "dev-tcp", `/dev/tcp/`},
	{"remote_code", "netcat-exec", `\b(nc|ncat)\b[^;|&]*\s-e\b`},
	{"remote_code", "socat-exec", `\bsocat\b[^;|&]*\b(exec|system)\b`},
	{"remote_code", "pipe-to-shell", `(curl|wget)[^;|&]{0,200}\|\s*(sudo\s+)?(ba|z|fi)?sh\b`},
	{"remote_code", "eval-fetch", `eval\s*\(?\s*["']?\$\(?(curl|wget)`},
	{"remote_code", "mkfifo-shell", `mkfifo\b`},
	{"remote_code", "interactive-shell-redirect", `\b(ba|z)?sh\s+-i\s+>&`},

	// --- persistence / backdoors ---
	{"persistence", "crontab", `crontab\s+(-e|-r|install)\b`},
	{"persistence", "systemd-unit-write", `(>>?|tee|cp|mv|install)\b[^;|&]{0,40}/etc/systemd`},
	{"persistence", "shell-rc-write", `(?:^|[^a-z0-9])(bashrc|zshrc|bash_profile)\b`},
	{"persistence", "authorized-keys-write", `authorized_keys\b`},
	{"persistence", "user-creation", `\b(useradd|adduser)\b`},

	// --- credential minting for another party ---
	{"credential_abuse", "mint-key", `\b(create|mint|generate|issue|make)\b[^.]{0,40}\b(api[_ -]?keys?|access[_ -]?tokens?|service[_ -]?account[_ -]?keys?|credentials?)\b[^.]{0,30}\b(for|and\s+send|and\s+share|and\s+give)\b`},

	// --- instruction override / persona jailbreak ---
	{"instruction_override", "override-previous", `\b(ignore|disregard|forget|override|bypass|skip)\b[^.]{0,30}\b(previous|prior|earlier|all|above|your|the)\b[^.]{0,25}\b(instructions?|rules?|policies?|guardrails?|directives?|filters?|restrictions?|system\s+prompt)\b`},
	{"instruction_override", "persona-jailbreak", `\byou\s+are\s+now\b[^.]{0,40}\b(developer\s+mode|dan\b|unrestricted|unfiltered|jailbroken|without\s+(restrictions|limits|filters|rules))\b`},
	{"instruction_override", "act-as-authority", `\b(act|behave|pretend|roleplay)\s+as\b[^.]{0,30}\b(an?\s+)?(unrestricted|unfiltered|root|admin|the\s+(user|human|owner|administrator))\b`},
	{"instruction_override", "treat-as-human", `\b(treat|consider|regard)\b[^.]{0,30}\b(this|the\s+above|these)\b[^.]{0,25}\b(as|like)\b[^.]{0,30}\b(from|written\s+by|sent\s+by)\b[^.]{0,25}\b(the\s+)?(user|human|owner|admin|system)\b`},
	{"instruction_override", "new-instructions", `\b(new|updated|revised|higher[_-]priority)\s+(instructions?|directives?|policies)\b\s*[:\-]`},
	{"instruction_override", "i-am-owner", `\bi\s+am\s+(the|your)\s+(owner|admin|administrator|creator|human\s+user)\b`},
	{"instruction_override", "user-approved-action", `(?:^|[^a-z0-9])(the\s+)?(user|human|owner)\s+(has\s+)?(approved|authorized|instructed|asked|said)\b[^.]{0,40}(run|running|execute|executing|delete|deleting|remove|removing|drop|dropping|send|sending|upload|uploading|install|installing)\b`},

	// --- encoding / filter evasion ---
	{"encoding_evasion", "base64-decode", `base64\s+(-d|--decode)\b`},
	{"encoding_evasion", "decode-this", `\b(decode|decrypt)\b[^.]{0,30}\b(this|the\s+(following|above|below|message|text|payload))\b`},
	{"encoding_evasion", "respond-in-encoding", `\brespond\s+(only\s+)?in\s+(base64|hex|rot13|cipher|encrypted\s+text)\b`},
	{"encoding_evasion", "eval-blob", `\beval\b\s*\(?\s*["']?[A-Za-z0-9+/=]{40,}`},
	{"encoding_evasion", "xxd-reverse", `xxd\s+-r\b`},
	{"encoding_evasion", "openssl-decrypt", `openssl\s+enc\b[^;|&]*-d\b`},

	// --- replication / worm behavior ---
	{"replication", "forward-to-all", `\b(send|forward|relay|propagate|broadcast|pass)\b[^.]{0,40}\b(this|the\s+above)\b[^.]{0,20}\b(message|instruction|text|payload)\b[^.]{0,40}\b(all|every|each)\b[^.]{0,20}\b(agents?|sessions?|peers?|members?|namespaces?)\b`},
	{"replication", "send-this-to-all", `\b(send|forward)\b[^.]{0,30}\bthis\b[^.]{0,40}\bto\s+all\b`},

	// --- supply chain ---
	{"supply_chain", "install-from-url", `(pip3?|npm|yarn|pnpm)\s+install\b[^;|&]*(https?://|git\+https?)`},
	{"supply_chain", "git-url-rewrite", `git\s+config\b[^;|&]*insteadof`},
}

// ContentFilterConfig is the yaml/env configuration for the policy.
type ContentFilterConfig struct {
	// Mode: "block" (default - matching messages are rejected) or "off".
	Mode string `yaml:"mode"`
	// Namespaces lists namespaces where the filter is skipped entirely
	// (security-research coordination namespaces, for example).
	Namespaces []string `yaml:"namespaces"`
	// ExtraPatterns are additional Go regex sources appended to the
	// registry under the "custom" category.
	ExtraPatterns []string `yaml:"extra_patterns"`
}

// NewContentPolicy compiles the registry plus any extra patterns.
// It returns nil (no filtering) when mode is "off" or empty-with-off.
func NewContentPolicy(cfg ContentFilterConfig) (*ContentPolicy, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" || mode == "off" {
		return nil, nil
	}
	if mode != "block" {
		return nil, fmt.Errorf("content filter: unknown mode %q (want block or off)", cfg.Mode)
	}
	p := &ContentPolicy{skipNS: make(map[string]bool, len(cfg.Namespaces))}
	for _, ns := range cfg.Namespaces {
		p.skipNS[ns] = true
	}
	for _, s := range contentPatternSpecs {
		re, err := regexp.Compile(`(?i)` + s.pattern)
		if err != nil {
			return nil, fmt.Errorf("content filter: %s/%s: %w", s.category, s.name, err)
		}
		p.patterns = append(p.patterns, contentPattern{category: s.category, name: s.name, re: re})
	}
	for i, raw := range cfg.ExtraPatterns {
		re, err := regexp.Compile(`(?i)` + raw)
		if err != nil {
			return nil, fmt.Errorf("content filter: extra_patterns[%d]: %w", i, err)
		}
		p.patterns = append(p.patterns, contentPattern{category: "custom", name: fmt.Sprintf("extra-%d", i), re: re})
	}
	return p, nil
}

// Check reports whether a message body must be blocked. The returned
// category and pattern name are safe for the error surface (they never
// echo message content). An empty category means the body is allowed.
func (p *ContentPolicy) Check(namespace, body string) (category, name string) {
	if p == nil || p.skipNS[namespace] {
		return "", ""
	}
	for _, cp := range p.patterns {
		if cp.re.MatchString(body) {
			return cp.category, cp.name
		}
	}
	return "", ""
}
