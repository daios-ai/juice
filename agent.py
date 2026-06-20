#!/usr/bin/env python3
"""
agent.py — an OpenCode-style coding agent that builds and publishes a Juice WASM action.

This is the OpenCode loop adapted for Juice: ONE loop that maintains the full conversation and lets
the model choose tool calls repeatedly (Gather / Act / Verify are emergent, not hardcoded phases).
Each turn the model (@sys/llm/chat) emits exactly one tool call; the harness executes it via the
`juice` CLI, appends the result to the conversation, and loops. The model writes a TinyGo `Handle`
using the Juice WASM SDK, compiles it (@sys/tinygo/compile), tests it by actually running it, fixes
its own code from the exact compiler/runtime errors it sees, and finally publishes it.

Unlike @sys/make (a stateless pipeline that regenerates from scratch each attempt), the conversation
here is never reset — the model repairs its previous code in place. Verification is real kernel
execution, and nothing is published until it runs correctly 3 times.

USAGE
    AGENT_PASSWORD=demo python3 agent.py "translate English into Chilean Spanish with heavy slang"

CONFIG (environment)
    JUICE_BIN        path to the juice binary           (default: ./juice)
    JUICE_DB         SQLite db path                      (default: juice.db)
    AGENT            handle the loop runs as             (default: @agent)
    AGENT_PASSWORD   if set, auto-login as AGENT first   (default: unset — assumes logged in)
    MAX_ITERS        cap on model tool-call turns        (default: 14)
    JUICE_AGENT_OUT  dir for saved synthesized code      (default: ./.agent_build)
    VERBOSE          if set, dump full payloads to stderr

REQUIREMENTS: a live Juice kernel reachable via the CLI against a local SQLite db, with a working
Ollama (chat) and TinyGo toolchain. Manual demo/research tool — NOT part of `go test ./...`.

OUTPUT: the published action ref is printed to stdout; all progress/log lines go to stderr.
"""

import json
import os
import re
import subprocess
import sys
import tempfile

JUICE_BIN = os.environ.get("JUICE_BIN", "./juice")
JUICE_DB = os.environ.get("JUICE_DB", "juice.db")
AGENT = os.environ.get("AGENT", "@agent")
AGENT_PASSWORD = os.environ.get("AGENT_PASSWORD")
MAX_ITERS = int(os.environ.get("MAX_ITERS", "14"))
BUILD_DIR = os.environ.get("JUICE_AGENT_OUT", ".agent_build")
VERBOSE = bool(os.environ.get("VERBOSE"))

REQUIRED_TESTS = 3       # distinct successful test inputs required before publish
RESULT_TRUNC = 3000      # cap each tool result fed back into the conversation
CALL_TIMEOUT = 180       # seconds per CLI call (LLM calls can be slow)

SYSTEM = r"""You are a coding agent on the Juice platform. Your job: given a task, write a TinyGo
action, compile it, test it by actually running it, and publish it as a public action. You work in a
loop — each turn you emit EXACTLY ONE tool call, receive its result, and continue. Keep your work
across turns: when a compile or test fails, FIX your previous code using the exact error. Never start
over from scratch.

THE CODE YOU WRITE (Juice WASM SDK):
- Write ONLY: func Handle(in map[string]any) (map[string]any, error)   (plus any private helpers).
- The SDK already provides: package, imports, run, main, alloc. NEVER write `package main`, NEVER write
  import statements, NEVER write func run/main/alloc, //export, or unsafe.
- Pre-imported and ready to use: encoding/json, errors, math, sort, strconv, strings. There is NO fmt —
  use errors.New("...") instead of fmt.Errorf.
- Do NOT redeclare these SDK helpers; you may CALL them:
    JuiceCall(action string, args []byte) ([]byte, error)   // call another Juice action
    mustMarshal(v any) []byte                                // JSON-encode
    JuiceLog(level, msg string)
- Input: JSON numbers arrive as float64, text as string. Read fields from `in`, e.g.
    x, _ := in["x"].(float64)
    name, _ := in["name"].(string)
- Return (map[string]any{...}, nil) on success; return (nil, errors.New("reason")) to fail.
- There is NO network and NO filesystem. You CANNOT call OpenAI or any external API. To use an LLM,
  call a Juice action with JuiceCall:
    * Free-form text (translate / summarize / rewrite) -> @sys/llm/chat:
        args := mustMarshal(map[string]any{
            "messages": []any{ map[string]any{"role": "user", "content": prompt} },
        })
        reply, err := JuiceCall("@sys/llm/chat", args)
        if err != nil { return nil, err }
        var r struct{ Message struct{ Content string `json:"content"` } `json:"message"` }
        json.Unmarshal(reply, &r)   // r.Message.Content is the text
    * Structured output (a number/record/list) -> @sys/llm/json with an "output_schema".
- Never call your own action (no self-recursion).

TOOLS — emit exactly one per turn, as a fenced ```json block:
1) lookup — find existing Juice actions you can call.
   ```json
   {"tool":"lookup","args":{"query":"translate text"}}
   ```
2) compile — compile your TinyGo Handle. Put the json block first, THEN the code in a go block:
   ```json
   {"tool":"compile"}
   ```
   ```go
   func Handle(in map[string]any) (map[string]any, error) {
       // ...
   }
   ```
3) test — run your most recently compiled action on one input and see what it returns:
   ```json
   {"tool":"test","args":{"input":{"sentence":"hello"}}}
   ```
4) publish — register your tested action as a public action. Requires at least 3 successful, distinct
   test runs on the CURRENT compiled version. Provide a kebab-case name and typed JSON schemas:
   ```json
   {"tool":"publish","args":{"name":"chilean-slang","description":"...","input_schema":{"type":"object","properties":{"sentence":{"type":"string","description":"English sentence"}},"required":["sentence"]},"output_schema":{"type":"object","properties":{"chilean":{"type":"string","description":"Chilean slang translation"}},"required":["chilean"]}}}
   ```

RULES:
- Output ONLY the tool call: one ```json block (and, for compile, one ```go block). No other prose.
- Typical flow: (optionally lookup) -> compile -> fix compile errors -> test 3+ distinct inputs ->
  publish. But YOU decide each step from the results you see.
- When compile fails, read the diagnostics and emit compile again with corrected code.
- When a test output is wrong or errors, fix the code, compile, and test again.
- The action's price/funding is handled for you — do not worry about it."""


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def vlog(label, obj):
    if VERBOSE:
        log(f"  {label}: {json.dumps(obj)[:2000]}")


def clean_err(stderr, rc):
    """Extract the kernel's error message from CLI stderr, dropping log/usage noise."""
    for line in reversed([l.strip() for l in (stderr or "").splitlines() if l.strip()]):
        if line.startswith("Error:"):
            return line[len("Error:"):].strip()
    return f"exit {rc}"


def jrun(action, args):
    """Run a Juice action via `juice run`. Returns (ok, payload): on success the action's `result`
    object; on failure {"error", "exit_code"}. Args go through a temp @file.json (the conversation
    can grow large — an inline argv would blow ARG_MAX)."""
    fd, path = tempfile.mkstemp(suffix=".json", prefix="agent-args-")
    with os.fdopen(fd, "w") as f:
        json.dump(args, f)
    argv = [JUICE_BIN, "--db", JUICE_DB, "--json", "run", action, "@" + path]
    try:
        p = subprocess.run(argv, capture_output=True, text=True, timeout=CALL_TIMEOUT)
    except subprocess.TimeoutExpired:
        return False, {"error": f"call to {action} timed out after {CALL_TIMEOUT}s", "exit_code": -1}
    finally:
        try:
            os.unlink(path)
        except OSError:
            pass
    if p.returncode != 0:
        return False, {"error": clean_err(p.stderr, p.returncode), "exit_code": p.returncode}
    try:
        return True, json.loads(p.stdout).get("result", {})
    except json.JSONDecodeError:
        return False, {"error": f"unparseable output: {p.stdout[:300]}", "exit_code": 0}


def juice_cli(*args):
    """Run a non-`run` juice subcommand (action create/enable/update/delete/show). Returns
    (returncode, stdout, stderr)."""
    p = subprocess.run([JUICE_BIN, "--db", JUICE_DB, "--json", *args],
                       capture_output=True, text=True, timeout=CALL_TIMEOUT)
    return p.returncode, p.stdout, p.stderr


def ensure_login():
    if AGENT_PASSWORD:
        p = subprocess.run([JUICE_BIN, "--db", JUICE_DB, "auth", "login", AGENT,
                            "--password", AGENT_PASSWORD], capture_output=True, text=True,
                           timeout=CALL_TIMEOUT)
        if p.returncode != 0:
            log(f"login failed: {(p.stderr or '').strip()}")
            sys.exit(1)
    me = subprocess.run([JUICE_BIN, "--db", JUICE_DB, "--json", "user", "me"],
                        capture_output=True, text=True, timeout=CALL_TIMEOUT)
    if me.returncode != 0:
        log(f"not authenticated (run: juice auth login {AGENT}, or set AGENT_PASSWORD)")
        sys.exit(1)
    log(f"running as {json.loads(me.stdout).get('handle', '?')}")


def slugify(s, maxlen=32):
    s = re.sub(r"[^a-z0-9]+", "-", s.lower()).strip("-")
    return (s[:maxlen].strip("-")) or "action"


def free_name(base):
    """Resolve a free action name for AGENT: the clean `base`, else `base-2`, `base-3`, … up to
    `base-100` — the first not taken by a live action. Mirrors @sys/make's name allocation
    (make.go); soft-deleted names don't count (action show returns not-found for them)."""
    name = base
    for suffix in range(2, 101):
        rc, _, _ = juice_cli("action", "show", f"{AGENT}/{name}")
        if rc != 0:           # not found -> free
            return name
        name = f"{base}-{suffix}"
    return name


def extract_json(s):
    """First balanced {...} object in s (skips prose/fences)."""
    start = s.find("{")
    if start < 0:
        return ""
    depth = 0
    for j in range(start, len(s)):
        if s[j] == "{":
            depth += 1
        elif s[j] == "}":
            depth -= 1
            if depth == 0:
                return s[start:j + 1]
    return ""


def extract_go_block(s):
    """The first ```go ... ``` block (the compile source)."""
    i = s.find("```go")
    if i < 0:
        return ""
    rest = s[i + len("```go"):]
    if rest.startswith("\n"):
        rest = rest[1:]
    j = rest.find("```")
    return (rest[:j] if j >= 0 else rest).strip()


SUPPORTED_TYPES = {"object", "array", "string", "integer", "number", "boolean"}
SCHEMA_KEYS = ["type", "nullable", "enum", "description", "properties", "required", "items"]


def sanitize_schema(schema):
    """Keep only kernel-accepted keywords, drop bad types, give every property a description — so the
    result passes ValidateSchema. Mirrors make.go's sanitizeSchemaForRegistration."""
    if not isinstance(schema, dict):
        return {}
    out = {k: schema[k] for k in SCHEMA_KEYS if k in schema}
    t = out.get("type")
    if t is not None and (not isinstance(t, str) or t not in SUPPORTED_TYPES):
        out.pop("type", None)
    props = out.get("properties")
    if isinstance(props, dict):
        clean = {}
        for pname, child in props.items():
            s = sanitize_schema(child if isinstance(child, dict) else {})
            s.setdefault("description", pname)
            clean[pname] = s
        out["properties"] = clean
    if isinstance(out.get("items"), dict):
        out["items"] = sanitize_schema(out["items"])
    return out


JUICE_CALL_RE = re.compile(r'JuiceCall\("(@[^"]+)"')


def compute_price(source):
    """Sum the prices of the distinct actions the source JuiceCalls — the subtree bound the registered
    action needs so its internal subcalls are funded (validated: price 0 traps at runtime). Mirrors
    @sys/make's computePrice."""
    total = 0
    for ref in sorted(set(JUICE_CALL_RE.findall(source))):
        rc, out, _ = juice_cli("action", "show", ref)
        if rc == 0:
            try:
                total += int(json.loads(out).get("price", 0))
            except (json.JSONDecodeError, ValueError, TypeError):
                pass
    return total


def truncate(obj):
    s = obj if isinstance(obj, str) else json.dumps(obj)
    return s if len(s) <= RESULT_TRUNC else s[:RESULT_TRUNC] + "…(truncated)"


def parse_tool_call(reply):
    """Return (tool, args, source) from a model turn, or (None, None, None) if unparseable."""
    js = extract_json(reply)
    if not js:
        return None, None, None
    try:
        obj = json.loads(js)
    except json.JSONDecodeError:
        return None, None, None
    tool = obj.get("tool")
    args = obj.get("args") if isinstance(obj.get("args"), dict) else {}
    source = None
    if tool == "compile":
        source = extract_go_block(reply) or args.get("source")
    return tool, args, source


def temp_action(state, h):
    """Register (once) the compiled artifact `h` as a hidden, enabled, priced private action for
    testing, and return its ref."""
    if h in state["temp_refs"]:
        return state["temp_refs"][h]
    name = "__agent_tmp_" + h[:6]
    os.makedirs(BUILD_DIR, exist_ok=True)
    b64_path = os.path.join(BUILD_DIR, name + ".wasm.b64")
    with open(b64_path, "w") as f:
        f.write(state["artifacts"][h])
    price = state["prices"][h]
    rc, out, err = juice_cli("action", "create", name, "--kind", "wasm", "--artifact", b64_path,
                             "--price", str(price), "--description", "agent smoke-test (temporary)")
    ref = f"{AGENT}/{name}"
    if rc == 0:
        try:
            ref = json.loads(out).get("action", ref)
        except json.JSONDecodeError:
            pass
    juice_cli("action", "enable", ref)
    state["temp_refs"][h] = ref
    return ref


def execute_tool(tool, args, source, state):
    if tool == "lookup":
        q = (args.get("query") or "").strip() or state["task"]
        ok, res = jrun("@sys/lookup", {"query": q, "limit": 8})
        if not ok:
            return {"error": res.get("error")}
        return {"actions": [{"action": r.get("action"), "description": r.get("description"),
                             "input_schema": r.get("input_schema")} for r in res.get("results", [])]}

    if tool == "compile":
        if not source or not source.strip():
            return {"error": "no source — include the code in a ```go block with func Handle"}
        ok, res = jrun("@sys/tinygo/compile", {"source": source})
        if not ok:
            return {"error": res.get("error")}  # kernel error (e.g. toolchain missing)
        if res.get("status") == "success":
            h = res["artifact_hash"]
            state["artifacts"][h] = res["artifact"]
            state["sources"][h] = source
            state["prices"][h] = compute_price(source)
            state["passes"].setdefault(h, set())
            state["latest"] = h
            return {"ok": True, "artifact_hash": h[:12],
                    "note": f"compiled (price computed: {state['prices'][h]}). Now test it on inputs."}
        return {"ok": False, "diagnostics": res.get("diagnostics")}

    if tool == "test":
        h = state.get("latest")
        if not h:
            return {"error": "nothing compiled yet — compile first"}
        ref = temp_action(state, h)
        inp = args.get("input")
        if not isinstance(inp, dict):
            inp = {}
        ok, out = jrun(ref, inp)
        if ok:
            state["passes"][h].add(json.dumps(inp, sort_keys=True))
            return {"ran": True, "input": inp, "output": out,
                    "distinct_passes": len(state["passes"][h])}
        return {"ran": False, "input": inp, "error": out.get("error")}

    if tool == "publish":
        h = state.get("latest")
        if not h:
            return {"error": "nothing compiled yet"}
        npass = len(state["passes"].get(h, set()))
        if npass < REQUIRED_TESTS:
            return {"error": f"publish blocked: only {npass}/{REQUIRED_TESTS} distinct successful "
                             f"tests on the current version. Test more distinct inputs first."}
        name = free_name(slugify(args.get("name") or state["task"]))
        desc = (args.get("description") or state["task"])[:200]
        in_schema = sanitize_schema(args.get("input_schema") or {})
        out_schema = sanitize_schema(args.get("output_schema") or {})
        price = state["prices"][h]

        os.makedirs(BUILD_DIR, exist_ok=True)
        b64_path = os.path.join(BUILD_DIR, name + ".wasm.b64")
        with open(b64_path, "w") as f:
            f.write(state["artifacts"][h])
        create_args = ["action", "create", name, "--kind", "wasm", "--artifact", b64_path,
                       "--price", str(price), "--description", desc]
        if in_schema:
            create_args += ["--input-schema", json.dumps(in_schema)]
        if out_schema:
            create_args += ["--output-schema", json.dumps(out_schema)]
        rc, out, err = juice_cli(*create_args)
        if rc != 0:
            return {"error": "create failed: " + clean_err(err, rc)}
        ref = json.loads(out).get("action", f"{AGENT}/{name}")
        rc, out, err = juice_cli("action", "enable", ref)
        if rc != 0:
            juice_cli("action", "delete", ref)
            return {"error": "enable failed: " + clean_err(err, rc)}
        # Re-validate the passing inputs now WITH the typed schemas (output validation is active).
        for inp_json in list(state["passes"][h])[:REQUIRED_TESTS]:
            ok, o = jrun(ref, json.loads(inp_json))
            if not ok:
                juice_cli("action", "delete", ref)
                return {"error": f"validation failed under your declared schemas on input {inp_json}: "
                                 f"{o.get('error')}. Fix the schema or code, recompile, retest."}
        juice_cli("action", "update", ref, "--public=true")
        header = (f"// Synthesized by agent.py for task: {state['task']}\n"
                  f"// Handle body; @sys/tinygo/compile prepends the Juice WASM SDK.\n\n")
        with open(os.path.join(BUILD_DIR, name + ".go"), "w") as f:
            f.write(header + state["sources"][h].rstrip("\n") + "\n")
        with open(os.path.join(BUILD_DIR, name + ".contract.json"), "w") as f:
            json.dump({"description": desc, "price": price, "input_schema": in_schema,
                       "output_schema": out_schema}, f, indent=2)
        state["published"] = ref
        return {"ok": True, "action": ref, "price": price}

    if tool == "done":
        state["done"] = True
        return {"done": True}

    return {"error": f"unknown tool '{tool}'"}


def cleanup(state):
    for h, ref in state["temp_refs"].items():
        juice_cli("action", "delete", ref)
        b64_path = os.path.join(BUILD_DIR, "__agent_tmp_" + h[:6] + ".wasm.b64")
        try:
            os.unlink(b64_path)
        except OSError:
            pass


def main():
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        log('usage: python3 agent.py "<task: the action to build>"')
        sys.exit(2)
    task = sys.argv[1].strip()
    ensure_login()
    log(f"task: {task}")

    state = {"task": task, "artifacts": {}, "sources": {}, "prices": {}, "passes": {},
             "temp_refs": {}, "latest": None, "published": None, "done": False}
    messages = [
        {"role": "system", "content": SYSTEM},
        {"role": "user", "content": f"Task: {task}\n\nBuild and publish a Juice action for this. "
                                    f"Begin."},
    ]

    for i in range(MAX_ITERS):
        ok, res = jrun("@sys/llm/chat", {"messages": messages})
        if not ok:
            log(f"[{i+1}] chat failed: {res.get('error')}")
            break
        reply = (res.get("message") or {}).get("content", "")
        tool, args, source = parse_tool_call(reply)
        if not tool:
            log(f"[{i+1}] no tool call parsed — nudging")
            messages.append({"role": "assistant", "content": reply})
            messages.append({"role": "user", "content": "Respond with exactly one tool call as a "
                             "```json block (plus a ```go block for compile). No other text."})
            continue

        summary = "" if tool == "compile" else json.dumps(args)[:100]
        log(f"[{i+1}] {tool} {summary}")
        result = execute_tool(tool, args, source, state)
        vlog("result", result)
        # concise human trace of the outcome
        if tool == "compile":
            if result.get("ok"):
                log(f"      compiled ✓ price={state['prices'][state['latest']]}")
            else:
                d = result.get("diagnostics") or [result.get("error", "")]
                log(f"      compile FAILED: {truncate(d[0] if d else '')[:200]}")
        elif tool == "test":
            if result.get("ran"):
                log(f"      ran ✓ ({result['distinct_passes']} distinct passes) -> "
                    f"{truncate(result.get('output'))[:160]}")
            else:
                log(f"      run FAILED: {truncate(result.get('error',''))[:200]}")
        elif tool == "publish":
            log(f"      {'PUBLISHED ' + result['action'] if result.get('ok') else 'publish rejected: ' + result.get('error','')}")

        messages.append({"role": "assistant", "content": reply})
        messages.append({"role": "user", "content": f"TOOL {tool} RESULT:\n{truncate(result)}"})

        if state["published"] or state["done"]:
            break
    else:
        log(f"reached MAX_ITERS={MAX_ITERS}")

    cleanup(state)
    if state["published"]:
        ref = state["published"]
        log(f"done: {ref}")
        print(f"Published public action: {ref}\nRun it: juice run {ref} '<json>'")
    else:
        log("no action published")
        print("FAILED to synthesize a working action — see the stderr trace.")
        sys.exit(1)


if __name__ == "__main__":
    main()
