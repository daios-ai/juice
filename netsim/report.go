// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }

// Snapshot is everything one kernel knows at the end of the run. The report is computed from these
// rather than from anything the story remembered, so a story that lied about what it did cannot
// produce a passing report.
type Snapshot struct {
	Identity  map[string]any   `json:"identity"`
	Peers     []map[string]any `json:"peers"`
	Users     []map[string]any `json:"users"`
	Actions   []map[string]any `json:"actions"`
	Txs       []map[string]any `json:"txs"`
	Steps     []map[string]any `json:"steps"`
	Processes []map[string]any `json:"processes"`
	// Owed is what this kernel is still waiting to be paid for, each obligation naming the buyer.
	// It is the seller's own record, which is the only side an obligation is kept on.
	Owed []map[string]any `json:"owed"`
}

// collect reads every kernel, and fails closed. A snapshot that could not be read is not an empty
// economy: it is an unmeasured one, and a checker given empty rows will happily report that
// everything balances. Every read that fails becomes a blocking entry instead.
func collect(n *Net) (map[string]Snapshot, []string) {
	out := map[string]Snapshot{}
	var blocking []string
	for name, k := range n.Kernels {
		if k.URL == "" {
			continue
		}
		s := Snapshot{}
		bad := func(what string, err error) {
			blocking = append(blocking, fmt.Sprintf("could not read %s from %s: %v", what, name, err))
		}
		op := "sysop-" + name
		var err error
		if s.Identity, err = read[map[string]any](k, op, "admin", "kernel", "show"); err != nil {
			bad("the kernel's own position", err)
		}
		if s.Peers, err = read[[]map[string]any](k, op, "admin", "peer", "list", "--all"); err != nil {
			bad("the peer rows", err)
		}
		if s.Users, err = read[[]map[string]any](k, op, "admin", "user", "list"); err != nil {
			bad("the accounts", err)
		}
		if s.Actions, err = read[[]map[string]any](k, op, "action", "list", "--all", "--limit", "200"); err != nil {
			bad("the catalogue", err)
		}
		if s.Steps, err = read[[]map[string]any](k, op, "step", "list", "--limit", "200"); err != nil {
			bad("the steps", err)
		}
		if s.Txs, err = pages(k, "sysop-"+name, "/v1/transactions"); err != nil {
			bad("the transactions", err)
		}
		if s.Processes, err = pages(k, "sysop-"+name, "/v1/processes"); err != nil {
			bad("the processes", err)
		}
		if awaiting, aerr := read[struct {
			Owed []map[string]any `json:"owed"`
		}](k, op, "admin", "kernel", "deposits"); aerr != nil {
			bad("what it is still owed", aerr)
		} else {
			s.Owed = awaiting.Owed
		}
		out[name] = s
	}
	return out, blocking
}

// pages reads every page of a listing. A report that says "every recorded transaction" must have
// read them all, and one page is capped at 200. A page that will not decode stops the read with an
// error rather than truncating it silently.
func pages(k *Kernel, actor, path string) ([]map[string]any, error) {
	var all []map[string]any
	for offset := 0; offset < 20000; offset += 200 {
		var page []map[string]any
		body := k.Get(actor, fmt.Sprintf("%s?limit=200&offset=%d", path, offset))
		if body == "" {
			return all, fmt.Errorf("%s returned nothing at offset %d", path, offset)
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			return all, fmt.Errorf("%s at offset %d: %w", path, offset, err)
		}
		all = append(all, page...)
		if len(page) < 200 {
			return all, nil
		}
	}
	return all, nil
}

func num(m map[string]any, field string) int64 {
	if f, ok := m[field].(float64); ok {
		return int64(f)
	}
	return 0
}

func str(m map[string]any, field string) string {
	s, _ := m[field].(string)
	return s
}

// Report is what the run decided, and why.
// Report is what the run decided, and why.
type Report struct {
	runInfo
	DurationS  float64           `json:"duration_s"`
	Rail       string            `json:"rail"`
	Rounds     int               `json:"rounds"`
	Operations int               `json:"operations"`
	Verdicts   map[string]bool   `json:"verdicts"`
	Metrics    map[string]any    `json:"metrics"`
	Expected   map[string]int    `json:"checks_as_specified"`
	Unexpected map[string]int    `json:"checks_not_as_specified"`
	Product    map[string]string `json:"product_failures"`
	RailCost   map[string]any    `json:"rail_cost"`
	Recovery   float64           `json:"recovery_s"`
	Throughput float64           `json:"throughput_calls_per_s"`
	Blocking   []string          `json:"insufficient_evidence"`
}

func Judge(n *Net, st *story, rounds int, railCost map[string]any) (*Report, error) {
	snaps, blocking := collect(n)
	b, _ := json.MarshalIndent(snaps, "", " ")
	_ = writeFile(filepath.Join(n.Root, "checkpoints", "final.json"), string(b))

	r := &Report{runInfo: n.Run, Rail: n.Rail.Name(), Rounds: rounds, Operations: n.Ops(),
		DurationS: time.Since(n.Run.StartedAt).Seconds(),
		Recovery:  st.recoverySeconds, Throughput: st.burstCallsPerSec,
		Verdicts: map[string]bool{}, Metrics: map[string]any{},
		Expected: n.Expected, Unexpected: n.Unexpected, Product: n.Defects, RailCost: railCost,
		Blocking: blocking}
	var md []string
	w := func(f string, a ...any) { md = append(md, fmt.Sprintf(f, a...)) }

	w("# Network simulation — %s rail", r.Rail)
	w("")

	// ---- evidence. A run that collected nothing must not report a pass.
	for name, s := range snaps {
		if len(s.Txs) == 0 && len(s.Users) == 0 {
			r.Blocking = append(r.Blocking, fmt.Sprintf("kernel %s reported no state at all", name))
		}
	}
	topLevel, nested, local, cross, other := actionCounts(snaps, st.owners)
	r.Metrics["actions"] = map[string]int{"top_level": topLevel, "nested": nested,
		"local": local, "cross_kernel": cross, "other_rows": other}
	if topLevel < 500 {
		r.Blocking = append(r.Blocking, fmt.Sprintf(
			"only %d top-level calls were executed; the full profile needs at least 500", topLevel))
	}
	if cross < 100 {
		r.Blocking = append(r.Blocking, fmt.Sprintf(
			"only %d calls crossed a kernel boundary; the full profile needs at least 100", cross))
	}
	w("%d kernels, %d trading rounds. %d top-level calls (%d local, %d cross-kernel), %d nested, "+
		"in %d commands.", len(snaps), rounds, topLevel, local, cross, nested, r.Operations)
	w("")

	// ---- the refund law, over every transaction tree.
	// Every call's money is accounted for by one identity, whatever happened inside it:
	//
	//     gross - refund  ==  fee + net + SUM over direct children of (gross - refund)
	//
	// What the caller paid equals what this call kept plus what it spent buying others. A failure
	// keeps nothing, so a failed parent with one settled child must refund exactly the price less
	// that child's cost — the partial refund — and a failure that consumed nothing refunds all of
	// it. Checking the identity on every transaction rather than on a chosen example means a run
	// cannot pass by never producing the interesting case.
	breaches, partials, composed, checked := refundLaw(snaps)
	r.Metrics["refund_law"] = map[string]any{"transactions_checked": checked,
		"violations": len(breaches), "partial_refunds_observed": partials,
		"transactions_with_a_parent": composed}
	w("## The refund law")
	w("")
	w("For every transaction, what the caller paid equals what the call kept plus what it spent on " +
		"the calls it made: `gross - refund == fee + net + sum(children)`.")
	w("")
	table(w, []string{"transactions checked", "violations", "partial refunds seen", "calls made inside another"},
		row(checked, len(breaches), partials, composed))
	if len(breaches) > 0 {
		var rows [][]string
		for _, b := range breaches[:min(10, len(breaches))] {
			rows = append(rows, row(b.Kernel, b.Action, b.Status, b.Paid, b.Kept, b.Spent))
		}
		table(w, []string{"kernel", "action", "status", "paid", "kept", "spent on children"}, rows...)
	}

	// ---- bilateral consistency. An obligation is one row on the serving side, so the test is what
	// each kernel says it is still owed — and that no peer row holds money at all, since under this
	// economy a peer account is identity and never a wallet.
	contradictions, outstanding := positions(snaps, n.Kernels)
	w("## Positions between kernels")
	w("")
	if len(outstanding) == 0 {
		w("Every obligation between kernels was settled and paid for.")
	} else {
		for _, o := range outstanding {
			w("- %s", o)
		}
	}
	for _, c := range contradictions {
		n.Product("positions", c)
	}
	w("")
	r.Metrics["unsettled_positions"] = len(outstanding)

	// ---- money that never came back. A call left parked is a caller whose funds are held with
	// nothing to show for it, which is the failure that matters most to a user.
	//
	// It is counted from the processes, not from transaction rows: a call still waiting for its
	// receipt has no settled transaction, so reading rows reports none parked at the exact moment
	// some are. This is the operator's own supervision view (§13).
	parked, parkedFunds := 0, int64(0)
	for _, sn := range snaps {
		c, f, _ := parkedIn(sn.Processes)
		parked += c
		parkedFunds += f
	}
	r.Metrics["calls_left_parked"] = parked
	r.Metrics["funds_parked"] = parkedFunds

	// ---- the checks the story made.
	w("## Checked behaviour")
	w("")
	table(w, []string{"behaved as specified", "did not"}, row(total(n.Expected), total(n.Unexpected)))
	if len(n.Defects) > 0 {
		w("### Failures in the system under test")
		w("")
		w("These are defects in what was tested, not in the test. They are listed apart because " +
			"the two need opposite responses: a fault in the harness is fixed and forgotten, while " +
			"one of these must survive being noticed.")
		w("")
		for _, k := range slices.Sorted(maps.Keys(n.Defects)) {
			w("- **%s** — %s", k, n.Defects[k])
		}
		w("")
	}
	if len(n.Unexpected) > len(n.Defects) {
		w("### Checks that did not behave as specified")
		w("")
		var rows [][]string
		for _, k := range slices.Sorted(maps.Keys(n.Unexpected)) {
			if _, isProduct := n.Defects[k]; !isProduct {
				rows = append(rows, row(k, n.Unexpected[k]))
			}
		}
		table(w, []string{"check", "times it did not behave as specified"}, rows...)
	}

	// ---- attacks. Each is answered only if the network refused it for the reason under test.
	attacks := []struct {
		want   string
		labels []string
	}{
		{"more unsecured credit than one identity could draw, by minting identities",
			[]string{"attack.sybil_reached_the_cap", "attack.sybil_shares_one_cap"}},
		{"paid work with no balance to pay for it",
			[]string{"attack.freerider_local", "attack.freerider_remote", "attack.freerider_locked_nothing"}},
		{"one payment credited more than once",
			[]string{"money.one_payment_credited_once"}},
		{"an action that was never exported",
			[]string{"attack.local_action_not_exported", "attack.private_action_not_exported",
				"attack.no_relay_through_a_third_kernel", "attack.value_may_not_cross"}},
		{"a name already in use by someone else",
			[]string{"attack.squatted_name_unmoved", "attack.squatted_name_still_buys"}},
	}
	w("## Attacks")
	w("")
	attackFailures := 0
	var attackRows [][]string
	for _, a := range attacks {
		good, bad := 0, 0
		for _, l := range a.labels {
			good += n.Expected[l]
			bad += n.Unexpected[l]
		}
		attackFailures += bad
		if good == 0 && bad == 0 {
			bad = 1 // an attack that never ran is not an attack that was refused
			attackFailures++
		}
		attackRows = append(attackRows, row(a.want, good, bad))
	}
	table(w, []string{"the attacker wanted", "held", "did not hold"}, attackRows...)

	// ---- evidence privacy, checked on what the kernel actually served.
	leaks, missing := evidenceLeaks(n.Root)
	r.Blocking = append(r.Blocking, missing...)
	w("## Evidence")
	w("")
	if len(leaks) == 0 {
		w("The ratings a peer serves and the catalogue served without credentials carry none of the " +
			"identifying or secret fields checked for.")
	} else {
		for _, l := range leaks {
			w("- %s", l)
		}
	}
	w("")

	// ---- latency, by what the call had to cross.
	classes := latencyByClass(readLog(n.Root))
	lat := map[string]any{}
	latencyOK := len(classes) > 0
	w("## Latency")
	w("")
	var latRows [][]string
	for _, c := range slices.Sorted(maps.Keys(classes)) {
		v := classes[c]
		p50, p95 := percentile(v, 0.5), percentile(v, 0.95)
		lat[c] = map[string]any{"n": len(v), "p50": p50, "p95": p95}
		if p95 >= 5000 {
			latencyOK = false
		}
		latRows = append(latRows, row(c, len(v), p50, p95))
	}
	table(w, []string{"calls that crossed", "number", "median (ms)", "95th percentile (ms)"}, latRows...)
	r.Metrics["latency_ms"] = lat

	// ---- the independent oracle ----
	deposited, held, perKernel := conservation(snaps, st.deposits)
	var conservationBreak []string
	if deposited != held {
		conservationBreak = append(conservationBreak, fmt.Sprintf(
			"%d entered the economy from outside and %d is held across every account: a difference of %d",
			deposited, held, held-deposited))
	}
	priceBreaks := priceFidelity(snaps, st.prices, st.owners, storyRates(), st.scale)
	settlementBreaks := settlementFidelity(st.opened)
	var wall []int64
	for _, o := range st.opened {
		if o.Closed {
			wall = append(wall, o.WallMs)
		}
	}
	r.Metrics["settlements"] = map[string]any{"count": len(st.opened),
		"wall_ms_p50": percentile(wall, 0.5), "wall_ms_max": percentile(wall, 1.0)}
	r.Metrics["oracle"] = map[string]any{
		"deposited": deposited, "held": held,
		"conservation_violations": len(conservationBreak),
		"price_violations":        len(priceBreaks),
		"settlements_checked":     len(st.opened),
		"settlement_violations":   len(settlementBreaks),
	}
	w("## Independent accounting")
	w("")
	w("Predicted from what the story did, checked against what the kernels report.")
	w("")
	table(w, []string{"property", "checked", "violations"},
		row("money entering equals money held", fmt.Sprintf("%d deposited, %d held", deposited, held), len(conservationBreak)),
		row("every call charged its advertised terms", fmt.Sprintf("%d transactions", checked), len(priceBreaks)),
		row("every obligation was settled by a payment of what was owed", fmt.Sprintf("%d obligations", len(st.opened)), len(settlementBreaks)))
	for _, line := range perKernel {
		w("- %s", line)
	}
	w("")
	for _, b := range append(append(append([]string{}, conservationBreak...), priceBreaks...), settlementBreaks...) {
		n.Product("oracle", b)
	}

	// ---- the verdict. Everything that can be decided is decided: a number that is only displayed
	// cannot fail a run, and a suite whose report is advisory is not a gate.
	r.Verdicts = map[string]bool{
		"evidence_sufficient":      len(r.Blocking) == 0,
		"refund_law_holds":         len(breaches) == 0,
		"no_contradictory_debts":   len(contradictions) == 0,
		"every_debt_settled":       len(outstanding) == 0,
		"no_call_left_parked":      parked == 0,
		"every_check_as_specified": len(n.Unexpected) == 0,
		"no_product_failure_found": len(n.Defects) == 0,
		"every_attack_refused":     attackFailures == 0,
		"no_evidence_leaked":       len(leaks) == 0,
		"composition_exercised":    composed > 0,
		"money_is_conserved":       len(conservationBreak) == 0,
		"prices_were_honoured":     len(priceBreaks) == 0,
		"settlements_were_exact":   len(settlementBreaks) == 0,
		"latency_within_bounds":    latencyOK,
	}
	if railErr, ok := railCost["exhausted"].(bool); ok {
		r.Verdicts["rail_paid_for_all_its_work"] = !railErr
	}

	var failed []string
	for _, k := range slices.Sorted(maps.Keys(r.Verdicts)) {
		if !r.Verdicts[k] {
			failed = append(failed, k)
		}
	}
	if len(r.Blocking) > 0 {
		w("## Insufficient evidence")
		w("")
		w("This run cannot be measured. The figures above describe whatever was collected, not the " +
			"economy that was meant to run.")
		w("")
		for _, b := range r.Blocking {
			w("- %s", b)
		}
		w("")
	}
	w("## Not judged here")
	w("")
	w("- the exactness of settlement arithmetic against an independent model")
	w("- whether an operator would find the refusal messages intelligible, which no script judges")
	w("")
	if len(failed) > 0 {
		w("## Result: FAIL — %s", strings.Join(failed, ", "))
	} else {
		w("## Result: PASS")
	}
	w("")

	jb, _ := json.MarshalIndent(r, "", "  ")
	_ = writeFile(filepath.Join(n.Root, "metrics.json"), string(jb))
	_ = writeFile(filepath.Join(n.Root, "report.md"), strings.Join(md, "\n")+"\n")
	if len(failed) > 0 {
		return r, fmt.Errorf("FAIL — %s", strings.Join(failed, ", "))
	}
	return r, nil
}

// actionCounts is what was actually executed, counted as the different things they are. One
// purchase can leave a record on two kernels and a composite leaves one per call it makes, so the
// number of rows is not the number of actions a user asked for. A proxy records the bare name of
// the action it bought, so a call is cross-kernel when the action lives on another kernel — not
// when the caller happened to type a reference with an @ in it.
func actionCounts(snaps map[string]Snapshot, owners map[string]string) (topLevel, nested, local, cross, other int) {
	for name, sn := range snaps {
		for _, t := range sn.Txs {
			switch {
			case str(t, "parent_trace_id") != "":
				nested++
			case str(t, "action_name") == "":
				other++
			default:
				topLevel++
				if owner := owners[str(t, "action_name")]; owner != "" && owner != name {
					cross++
				} else {
					local++
				}
			}
		}
	}
	return
}

// evidenceLeaks checks what the kernel actually served — the ratings a peer reads and the catalogue
// a stranger sees — for fields that identify a person or expose a secret. A projection that was
// never captured is reported as missing evidence, not as clean.
func evidenceLeaks(root string) (leaks, missing []string) {
	forbidden := []string{"user_id", "caller_handle", "rater", "password", "token",
		"private_key", "recovery", "wasm_artifact"}
	for _, f := range []struct{ file, what string }{
		{"ratings-projection.json", "the ratings a peer serves"},
		{"catalogue-anonymous.json", "the catalogue served without credentials"},
	} {
		body, err := os.ReadFile(filepath.Join(root, f.file))
		if err != nil {
			missing = append(missing, "never captured "+f.what+", so it was not checked for leaks")
			continue
		}
		if !json.Valid(body) {
			missing = append(missing, f.what+" was not valid JSON, so it was not checked for leaks")
			continue
		}
		for _, bad := range forbidden {
			if strings.Contains(string(body), `"`+bad+`"`) {
				leaks = append(leaks, f.what+" carries "+bad)
			}
		}
	}
	return leaks, missing
}

// latencyByClass groups successful calls by whether they crossed a kernel boundary.
func latencyByClass(recs []record) map[string][]int64 {
	classes := map[string][]int64{}
	for _, rec := range recs {
		if rec.Exit != 0 || !strings.Contains(rec.Cmd, " run ") {
			continue
		}
		class := "local"
		if strings.Contains(rec.Cmd, "@") {
			class = "cross-kernel"
		}
		classes[class] = append(classes[class], rec.Ms)
	}
	return classes
}

type breach struct {
	Kernel, Tx, Action, Status string
	Paid, Kept, Spent          int64
}

// refundLaw applies one identity to every transaction:
//
//	gross - refund  ==  fee + net + SUM over direct children of (gross - refund)
//
// What the caller paid equals what this call kept plus what it spent buying others. A failure keeps
// nothing, so a failed parent with one settled child must refund exactly the price less that
// child's cost — the partial refund — and a failure that consumed nothing refunds all of it.
// Checking every transaction rather than a chosen example means a run cannot pass by never
// producing the interesting case.
func refundLaw(snaps map[string]Snapshot) (breaches []breach, partials, composed, checked int) {
	for name, s := range snaps {
		children := map[string][]map[string]any{}
		for _, t := range s.Txs {
			if p := str(t, "parent_trace_id"); p != "" {
				children[p] = append(children[p], t)
				composed++
			}
		}
		for _, t := range s.Txs {
			paid := num(t, "gross") - num(t, "refund")
			kept := num(t, "fee") + num(t, "net")
			var spent int64
			for _, c := range children[str(t, "trace_id")] {
				spent += num(c, "gross") - num(c, "refund")
			}
			checked++
			if paid != kept+spent {
				breaches = append(breaches, breach{name, str(t, "id"), str(t, "action_name"),
					str(t, "status"), paid, kept, spent})
			}
			if str(t, "status") == "failure" && spent > 0 && num(t, "refund") > 0 {
				partials++
			}
		}
	}
	return breaches, partials, composed, checked
}

// positions reads what every pair of kernels still owes each other, in both directions. An
// obligation is one row on the serving side, so it can be recorded on either kernel; looking at only
// one of them hides every debt owed by whichever name happens to sort first. A peer row that holds
// money at all is a contradiction under this economy — a peer account is identity, never a wallet —
// and is reported as one.
func positions(snaps map[string]Snapshot, kernels map[string]*Kernel) (contradictions, outstanding []string) {
	for a, sa := range snaps {
		for b := range snaps {
			if a == b {
				continue
			}
			if bal := peerBalance(sa, kernels[b]); bal != 0 {
				contradictions = append(contradictions,
					fmt.Sprintf("%s's row for %s holds %d; a peer account is never a wallet", a, b, bal))
			}
			if n := owedBy(sa, kernels[b]); n > 0 {
				outstanding = append(outstanding, fmt.Sprintf("%s still owes %s for %d calls", b, a, n))
			}
		}
	}
	sort.Strings(outstanding)
	sort.Strings(contradictions)
	return contradictions, outstanding
}

// parkedIn reads the processes still waiting for a receipt: how many, what they hold, and since
// when the oldest has waited.
func parkedIn(procs []map[string]any) (count int, funds int64, oldest string) {
	for _, pr := range procs {
		if awaiting, _ := pr["awaiting_receipt"].(bool); !awaiting {
			continue
		}
		count++
		funds += num(pr, "available") + num(pr, "locked")
		if since := str(pr, "awaiting_receipt_since"); since != "" && (oldest == "" || since < oldest) {
			oldest = since
		}
	}
	return count, funds, oldest
}

// row formats one table row from mixed values.
func row(cells ...any) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = fmt.Sprint(c)
	}
	return out
}

// table writes one markdown table.
func table(w func(string, ...any), header []string, rows ...[]string) {
	w("| %s |", strings.Join(header, " | "))
	w("|%s", strings.Repeat("---|", len(header)))
	for _, r := range rows {
		w("| %s |", strings.Join(r, " | "))
	}
	w("")
}

// owedBy is how many obligations one buyer owes this kernel and has not paid for, from the kernel's
// own record of what it is waiting to be paid.
func owedBy(s Snapshot, of *Kernel) int {
	if of == nil {
		return 0
	}
	n := 0
	for _, r := range s.Owed {
		// A kernel whose key could not be read names nothing: matching on an empty prefix would
		// count every row, and slicing one would panic while reporting a run that already failed.
		if of.Key == "" {
			return 0
		}
		if peer := str(r, "peer"); peer == of.Handle || peer == of.Key || strings.HasSuffix(peer, of.Key[:8]) {
			n++
		}
	}
	return n
}

// peerBalance is what a peer's row holds here, which under this economy is always nothing.
func peerBalance(s Snapshot, of *Kernel) int64 {
	if of == nil {
		return 0
	}
	for _, p := range s.Peers {
		if str(p, "public_key") == of.Key {
			return num(p, "available")
		}
	}
	return 0
}

func readLog(root string) []record {
	b, err := os.ReadFile(filepath.Join(root, "log.jsonl"))
	if err != nil {
		return nil
	}
	var out []record
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func percentile(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i]
}

func total(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

// ---- the independent economic oracle ---------------------------------------
//
// These three properties are predicted from what the story declared and checked against rows the
// story did not write. They are the difference between "the kernel agrees with itself", which the
// refund law shows, and "the kernel is right".

// Deposits is the money the story put into the economy from outside, in base units.
type Deposits struct {
	Total int64
	By    map[string]int64 // kernel -> credited
}

// conservation checks that money is neither created nor destroyed.
//
//	Σ external deposits  ==  Σ over every account on every kernel of (available + locked)
//
// It holds whether or not debts are open, which is what makes it worth checking at the end of a run
// that still has outstanding positions. A cross-kernel call sums to zero across the two kernels:
// the buyer loses q, its operator gains the import fee, the seller gains net and its operator gains
// fee + premium, and q = charge + premium + import fee with charge = fee + net. A settlement moves
// tokens between vaults and changes no account at all. So the only term that moves the total is a
// deposit, and the only accounts that hold it are the ones `admin users` lists — which includes
// `sys`, counted here exactly once, since the operator's earnings live in that account.
func conservation(snaps map[string]Snapshot, deposits Deposits) (int64, int64, []string) {
	var held int64
	var detail []string
	for name, s := range snaps {
		var k int64
		for _, u := range s.Users {
			k += num(u, "available") + num(u, "locked")
		}
		held += k
		detail = append(detail, fmt.Sprintf("%s holds %d against %d deposited", name, k, deposits.By[name]))
	}
	sort.Strings(detail)
	return deposits.Total, held, detail
}

// priceFidelity checks what each call actually charged against what its terms said it would.
//
// A local call charges its advertised price. A cross-kernel call charges the imported quote
// q = sr + ceil(sr·import_bps/10000) where sr = mp + ceil(mp·remote_bps/10000), computed from the
// seller's remote rate and the buyer's import rate. A local failure keeps nothing. A remote failure
// is the case that is easy to get wrong: the peer may have done paid work before failing, so the
// caller pays charge + premium from the signed receipt and the import fee is zero — not nothing at
// all (requirements.md §13).
func priceFidelity(snaps map[string]Snapshot, prices map[string]int64, owners map[string]string,
	rates map[string]kernelRates, scale int64) []string {
	var breaks []string
	for name, s := range snaps {
		for _, t := range s.Txs {
			if str(t, "parent_trace_id") != "" {
				continue // a child is bounded by its parent, not by an advertised price
			}
			action := str(t, "action_name")
			mp, known := prices[action]
			if !known {
				continue // natives, composites and the attackers' own actions have no fixed price
			}
			// A proxy records the bare name of the action it bought, not the reference the caller
			// typed, so remoteness is decided by where the action actually lives: a call recorded
			// on any kernel but the seller's crossed a boundary.
			seller := owners[action]
			remote := seller != "" && seller != name
			want := mp * scale
			if remote {
				sr := mp*scale + ceilDiv(mp*scale*int64(rates[seller].remoteBps), 10000)
				want = sr + ceilDiv(sr*int64(rates[name].importBps), 10000)
			}
			gross, refund := num(t, "gross"), num(t, "refund")
			switch str(t, "status") {
			case "success":
				if gross != want {
					breaks = append(breaks, fmt.Sprintf(
						"%s: a successful %s charged %d, but its terms said %d", name, action, gross, want))
				}
			case "failure":
				if !remote {
					if num(t, "fee")+num(t, "net") != 0 {
						breaks = append(breaks, fmt.Sprintf(
							"%s: a failed local %s kept %d", name, action, num(t, "fee")+num(t, "net")))
					}
					if gross-refund != 0 {
						breaks = append(breaks, fmt.Sprintf(
							"%s: a failed local %s charged %d", name, action, gross-refund))
					}
					continue
				}
				// A remote failure may legitimately charge: the peer did work and failed. What it
				// charged must be exactly what the peer's signed receipt says it drew, plus the
				// premium on that draw, and no import fee.
				paid := gross - refund
				charge, premium, ok := receiptDraw(t)
				if !ok {
					if paid != 0 {
						breaks = append(breaks, fmt.Sprintf(
							"%s: a failed remote %s charged %d with no receipt to justify it",
							name, action, paid))
					}
					continue
				}
				if paid != charge+premium {
					breaks = append(breaks, fmt.Sprintf(
						"%s: a failed remote %s charged %d, but its receipt drew %d with a premium of %d",
						name, action, paid, charge, premium))
				}
				if num(t, "fee") != 0 {
					breaks = append(breaks, fmt.Sprintf(
						"%s: a failed remote %s took an import fee of %d", name, action, num(t, "fee")))
				}
			}
		}
	}
	return breaks
}

// receiptDraw reads what the peer's signed receipt says it actually drew.
func receiptDraw(t map[string]any) (charge, premium int64, ok bool) {
	raw, is := t["remote_receipt_json"].(string)
	if !is || raw == "" {
		return 0, 0, false
	}
	var r map[string]any
	if json.Unmarshal([]byte(raw), &r) != nil {
		return 0, 0, false
	}
	return num(r, "charge"), num(r, "premium"), true
}

// settlementFidelity checks each obligation the story saw: it closed, and where the story caught the
// draw, the payment that closed it was not short. A paying draw pays either exactly what is owed or
// the whole face value, which is larger — never less, since a seller settled for less than it
// delivered is a seller robbed by the mechanism meant to pay it. An obligation whose draw the story
// never saw says nothing about the amount: it closed too quickly to read, or it lost and paid
// nothing, and neither is a break.
func settlementFidelity(opened []settlement) []string {
	var breaks []string
	for _, st := range opened {
		if !st.Closed {
			breaks = append(breaks, fmt.Sprintf(
				"the obligation of %d from %s to %s never closed", st.Obligation, st.Debtor, st.Creditor))
			continue
		}
		if st.Status == "announced" && st.Amount < st.Obligation {
			breaks = append(breaks, fmt.Sprintf(
				"an obligation of %d from %s to %s was settled by a payment of only %d",
				st.Obligation, st.Debtor, st.Creditor, st.Amount))
		}
	}
	return breaks
}

type kernelRates struct{ remoteBps, importBps int }

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
