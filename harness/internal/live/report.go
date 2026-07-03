package live

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/report"
)

// ReportConfig configures the `harness report` subcommand: where to read the
// Result (and optional per-client observations) and where to write the rendered
// Markdown.
type ReportConfig struct {
	InPath      string    // Result JSON input (required)
	ClientsPath string    // per-client observations JSON (optional, "" => omitted)
	OutPath     string    // Markdown destination ("" or "-" => stdout)
	GeneratedAt time.Time // report timestamp, injected so report.Render stays pure
}

// RunReport reads a Result (and optional PerClientObservations), schema-validates
// both against THIS build's supported versions, renders the Markdown report, and
// writes it out. It returns a process exit code:
//
//	0  - the report rendered successfully (a PASS, FAIL, or ERROR Result all
//	     render successfully; the verdict is CONTENT of the report, not an error).
//	!0 - an IO, decode, or schemaVersion error (nothing was rendered).
//
// NOTE for slice 8 (CI gate): the pipeline must gate on the MEASURE command's
// exit code (0 PASS / 1 FAIL / 2 ERROR), NOT on `harness report`'s - report
// exits 0 for a FAIL or ERROR Result because rendering itself succeeded.
func RunReport(cfg ReportConfig) (int, error) {
	data, err := os.ReadFile(cfg.InPath)
	if err != nil {
		return 3, fmt.Errorf("read result %s: %w", cfg.InPath, err)
	}
	var res model.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return 3, fmt.Errorf("decode result %s: %w", cfg.InPath, err)
	}
	// REV 5: reject a Result whose schemaVersion this build does not render,
	// naming BOTH the found and the expected version, and fail non-zero.
	if res.SchemaVersion != model.SchemaVersion {
		return 3, fmt.Errorf("unsupported schemaVersion %q in %s: this harness build renders %q",
			res.SchemaVersion, cfg.InPath, model.SchemaVersion)
	}

	var clients *report.PerClientObservations
	if cfg.ClientsPath != "" {
		cdata, err := os.ReadFile(cfg.ClientsPath)
		if err != nil {
			return 3, fmt.Errorf("read clients %s: %w", cfg.ClientsPath, err)
		}
		var pc report.PerClientObservations
		if err := json.Unmarshal(cdata, &pc); err != nil {
			return 3, fmt.Errorf("decode clients %s: %w", cfg.ClientsPath, err)
		}
		if pc.SchemaVersion != report.ClientsSchemaVersion {
			return 3, fmt.Errorf("unsupported schemaVersion %q in %s: this harness build renders %q",
				pc.SchemaVersion, cfg.ClientsPath, report.ClientsSchemaVersion)
		}
		clients = &pc
	}

	md := report.Render(res, clients, report.RenderOptions{GeneratedAt: cfg.GeneratedAt})
	if err := writeReport(md, cfg.OutPath); err != nil {
		return 3, err
	}
	return 0, nil
}

func writeReport(md, outPath string) error {
	if outPath == "" || outPath == "-" {
		_, err := os.Stdout.WriteString(md)
		return err
	}
	return os.WriteFile(outPath, []byte(md), 0o644)
}
