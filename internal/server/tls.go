package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// buildTLS produces the *tls.Config used by both the HTTPS frontend and the
// control listener. It returns nil in TLSOff mode, which the caller reads as
// "serve plain HTTP".
//
// Wildcard certificates require a DNS-01 challenge, so auto mode always solves
// over DNS and never needs port 80 reachable.
func buildTLS(_ context.Context, cfg Config, log *slog.Logger) (*tls.Config, error) {
	switch cfg.TLS.Mode {
	case TLSOff:
		return nil, nil

	case TLSFiles:
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load certificate: %w", err)
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
		}, nil

	case TLSAuto:
		solver, err := dnsProvider(cfg.TLS.DNSProvider, cfg.TLS.DNSAPIToken)
		if err != nil {
			return nil, err
		}

		magic := certmagic.NewDefault()
		magic.Storage = &certmagic.FileStorage{Path: filepath.Join(cfg.DataDir, "certmagic")}
		magic.Logger = certmagicLogger()

		issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
			CA:     acmeCA(cfg.TLS.CA),
			Email:  cfg.TLS.Email,
			Agreed: true,
			// Disable the challenges that cannot satisfy a wildcard so a
			// misconfigured DNS token fails loudly instead of silently
			// falling back and issuing nothing useful.
			DisableHTTPChallenge:    true,
			DisableTLSALPNChallenge: true,
			DNS01Solver: &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{
					DNSProvider: solver,
					// certmagic's defaults time out after two minutes, which is
					// not enough for every provider to distribute a record to
					// all of its nameservers. A timeout counts as a failed
					// authorization against a tight CA rate limit, so waiting
					// is much cheaper than retrying.
					PropagationDelay:   cfg.TLS.PropagationDelay(),
					PropagationTimeout: cfg.TLS.PropagationTimeout(),
				},
			},
		})
		magic.Issuers = []certmagic.Issuer{issuer}

		// The wildcard covers every tunnel subdomain and the API host. The apex
		// is excluded unless asked for: see TLSConfig.IncludeApex.
		names := cfg.ManagedNames()

		// Management is asynchronous on purpose. A DNS-01 challenge waits for a
		// TXT record to propagate, which takes tens of seconds to minutes, and
		// blocking here would mean nothing is listening for that whole time:
		// no control plane, no API, no way to see what the server is doing. The
		// TLS config below serves from certmagic's cache, so HTTPS starts
		// working the moment the certificate lands, and certmagic retries with
		// its own backoff if issuance fails.
		log.Info("managing certificate in the background",
			"names", names, "ca", issuer.CA,
			"propagation_delay", cfg.TLS.PropagationDelay(),
			"propagation_timeout", cfg.TLS.PropagationTimeout())
		if err := magic.ManageAsync(context.Background(), names); err != nil {
			log.Error("could not start certificate management; HTTPS will not work",
				"names", names, "err", err)
			log.Warn("if something else already terminates TLS on this host, use tls.mode off",
				"hint", "zerock init-config --behind-proxy")
		}

		tlsCfg := magic.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)
		return tlsCfg, nil
	}
	return nil, fmt.Errorf("unhandled tls mode %q", cfg.TLS.Mode)
}

// dnsProviders maps a config name onto its libdns implementation. Adding a
// provider means one entry here and one line in the config documentation; the
// rest of the ACME path is provider-agnostic.
var dnsProviders = map[string]func(token string) certmagic.DNSProvider{
	"cloudflare":   func(token string) certmagic.DNSProvider { return &cloudflare.Provider{APIToken: token} },
	"digitalocean": func(token string) certmagic.DNSProvider { return &digitalocean.Provider{APIToken: token} },
}

// SupportedDNSProviders lists the provider names the config accepts, sorted so
// error messages are stable.
func SupportedDNSProviders() []string {
	names := make([]string, 0, len(dnsProviders))
	for name := range dnsProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// dnsProvider builds the DNS-01 solver for the configured provider.
func dnsProvider(name, token string) (certmagic.DNSProvider, error) {
	build, ok := dnsProviders[name]
	if !ok {
		return nil, fmt.Errorf("tls.dns_provider %q is not supported; built in: %s",
			name, strings.Join(SupportedDNSProviders(), ", "))
	}
	if token == "" {
		return nil, fmt.Errorf("tls.dns_api_token is required for the %s provider", name)
	}
	return build(token), nil
}

// acmeCA resolves the directory URL, accepting "staging" as a shorthand for
// Let's Encrypt's staging endpoint.
func acmeCA(ca string) string {
	switch ca {
	case "":
		return certmagic.LetsEncryptProductionCA
	case "staging":
		return certmagic.LetsEncryptStagingCA
	default:
		return ca
	}
}

// certmagicLogger adapts certmagic's zap requirement. ACME problems are the
// hardest part of a self-hosted tunnel server to diagnose, so its output is
// kept rather than discarded.
func certmagicLogger() *zap.Logger {
	enc := zap.NewProductionEncoderConfig()
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(enc),
		zapcore.Lock(os.Stderr),
		zapcore.InfoLevel,
	)
	return zap.New(core).Named("acme")
}
