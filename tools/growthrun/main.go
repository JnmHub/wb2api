// Command growthrun drives the CN growth flow for one exported account file.
// It is the live counterpart of the panel's 「全自动跑一轮」: same internal/ops
// path, no panel login, so a single account export can be exercised from a
// terminal or a cron job.
//
//	go run ./tools/growthrun -file accounts.json -work ./run -all
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/ops"
	"wb2api/internal/pool"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
)

func main() {
	file := flag.String("file", "", "exported workbuddy-accounts json")
	work := flag.String("work", "", "scratch dir for settings/activity")
	dry := flag.Bool("dry", false, "only print the task snapshot")
	claimAll := flag.Bool("all", false, "run the whole flow")
	flag.Parse()
	if *file == "" || *work == "" {
		fmt.Fprintln(os.Stderr, "-file and -work are required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(doc.Accounts) == 0 {
		fmt.Fprintln(os.Stderr, "no accounts")
		os.Exit(1)
	}
	rec := accounts.FromMap(doc.Accounts[0], "", time.Now())
	p := pool.New([]*accounts.Record{rec})
	st := settings.Open(*work)
	log := &activity.Log{Dir: *work}
	c := &ops.Client{
		Pool: p, Up: upstream.NewManager(upstream.DefaultTimeouts()), Settings: st, Activity: log,
	}
	acc := p.All()[0]
	ctx := context.Background()
	if *dry || !*claimAll {
		tasks, err := c.GrowthTasks(ctx, acc)
		if err != nil {
			fmt.Println("GrowthTasks error:", err)
			os.Exit(1)
		}
		fmt.Printf("%d tasks\n", len(tasks))
		for _, t := range tasks {
			fmt.Printf("  %-34s %-12s %d/%d  +%d\n", t.Code, t.Status, t.Current, t.Target, t.RewardCredit)
		}
		if mp, err := c.GrowthTasksMP(ctx, acc); err == nil && len(mp) > 0 {
			n := 0
			for _, t := range mp {
				if !ops.IsMiniProgramTask(t.Code) {
					continue
				}
				n++
				fmt.Printf("  %-34s %-12s %d/%d  +%d\n", t.Code, t.Status, t.Current, t.Target, t.RewardCredit)
			}
			if n > 0 {
				fmt.Printf("%d mini-program tasks\n", n)
			}
		}
		if !*claimAll {
			return
		}
	}
	logs, earned := c.RunGrowthTasks(ctx, acc)
	for _, l := range logs {
		fmt.Println(l)
	}
	fmt.Printf("\n== earned +%d\n", earned)
}
