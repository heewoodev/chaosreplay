package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"chaosreplay/internal/fingerprint"
	"chaosreplay/internal/replay"
	"chaosreplay/internal/scenario"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: chaosreplay <validate|plan|record|verify> scenario.json [record.json]")
}

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	s, err := scenario.Load(os.Args[2])
	fail(err)
	switch os.Args[1] {
	case "validate":
		h, err := scenario.Hash(s)
		fail(err)
		fmt.Printf("valid scenario=%s hash=%s faults=%v\n", s.ID, h, scenario.StableFaultTypes(s))
	case "plan":
		p, err := replay.Build(s)
		fail(err)
		printJSON(p)
	case "record":
		if len(os.Args) != 4 {
			usage()
			os.Exit(2)
		}
		p, err := replay.Build(s)
		fail(err)
		env := fingerprint.Capture()
		planHash, err := replay.HashPlan(p)
		fail(err)
		r := replay.Record{Plan: p, PlanHash: planHash, RunID: time.Now().UTC().Format("20060102T150405.000000000Z"), Outcome: "planned", RecordedAt: time.Now().UTC(), Adapter: "plan-only/v1", Environment: env, EnvironmentHash: fingerprint.Hash(env)}
		b, err := json.MarshalIndent(r, "", "  ")
		fail(err)
		fail(os.WriteFile(os.Args[3], append(b, '\n'), 0644))
		fmt.Println(os.Args[3])
	case "verify":
		if len(os.Args) != 4 {
			usage()
			os.Exit(2)
		}
		b, err := os.ReadFile(os.Args[3])
		fail(err)
		var r replay.Record
		fail(json.Unmarshal(b, &r))
		fail(replay.VerifyRecord(s, r))
		fmt.Println("verified")
	default:
		usage()
		os.Exit(2)
	}
}

func printJSON(v any) { b, err := json.MarshalIndent(v, "", "  "); fail(err); fmt.Println(string(b)) }
func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
