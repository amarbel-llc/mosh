# Unified host:session Namespace v1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use eng:subagent-driven-development to implement this plan task-by-task.

**Goal:** Implement RFC 0001 — `posh [user@]host:[group/]session` attach-or-create over the roaming transport, the `:session` explicit-local form, end-to-end exit status via the datagram capability table, `posh list host:`, and remote-session completion.

**Architecture:** Transport composition (FDR 0001 architecture A): a remote-session target runs `posh-server new -- posh attach [-g G] S` over ssh; persistence stays in the session daemon. The grammar is a total typed parser (`Target::parse`); the wire change is a TLV capability table behind one reserved flags bit (0x02) in each direction, with `EXIT_STATUS` as the first negotiated capability.

**Tech Stack:** Rust (crates/posh), no new dependencies. Tests via `just debug-cargo test` (fast loop) and the hermetic `just test-rust` lane.

**Rollback:** Purely additive CLI (explicit `posh ssh` / `posh attach` forms unchanged); the wire change is capability-negotiated so mixed versions degrade to today's behavior. Single-revert.

**References:** `docs/rfcs/0001-target-grammar-and-capability-table.md` (normative), `docs/features/0001-unified-host-session-namespace.md`, `docs/plans/2026-06-11-host-session-namespace-design.md`.

---

### Task 1: Capability table module (`caps.rs`)

**Promotion criteria:** N/A (new module).

**Files:**
- Create: `crates/posh/src/remote/caps.rs`
- Modify: `crates/posh/src/remote/mod.rs` (add `pub mod caps;`)

**Step 1: Write the module with failing tests**

```rust
//! RFC 0001 §3: the TLV capability table that rides behind the EXTENSION
//! bit (0x02) of both datagram directions. Unknown ids are preserved on
//! decode and ignored by consumers; malformed tables reject the message.

use crate::util::{Error, Result};

/// Reserved flags bit (both directions): a capability table follows the
/// flags byte. Permanent; never reuse for anything else.
pub const FLAG_EXTENSION: u8 = 0x02;

/// Capability ids (RFC 0001 registry). 224..=255 are experimental-only.
pub const CAP_PROTOCOL_VERSION: u8 = 0;
pub const CAP_EXIT_STATUS: u8 = 1;

/// The post-table format version we implement (payload of
/// CAP_PROTOCOL_VERSION).
pub const PROTOCOL_VERSION: u8 = 1;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Cap {
    pub id: u8,
    pub payload: Vec<u8>,
}

/// count:u8, then count × (id:u8, len:u8, payload).
pub fn encode_table(caps: &[Cap]) -> Vec<u8> {
    let mut out = vec![caps.len() as u8];
    for c in caps {
        debug_assert!(c.payload.len() <= u8::MAX as usize);
        out.push(c.id);
        out.push(c.payload.len() as u8);
        out.extend_from_slice(&c.payload);
    }
    out
}

/// Parses a table from the head of `data`; returns the entries and the
/// number of bytes consumed. Bounds-checked: count/len are peer-controlled
/// (RFC 0001 security considerations) — truncation is an error, never an
/// over-read or panic.
pub fn decode_table(data: &[u8]) -> Result<(Vec<Cap>, usize)> {
    let Some(&count) = data.first() else {
        return Err(Error::from("capability table truncated"));
    };
    let mut caps = Vec::with_capacity(count as usize);
    let mut at = 1;
    for _ in 0..count {
        let (Some(&id), Some(&len)) = (data.get(at), data.get(at + 1)) else {
            return Err(Error::from("capability entry truncated"));
        };
        at += 2;
        let end = at + len as usize;
        let Some(payload) = data.get(at..end) else {
            return Err(Error::from("capability payload truncated"));
        };
        caps.push(Cap { id, payload: payload.to_vec() });
        at = end;
    }
    Ok((caps, at))
}

/// The table this build sends in every message: protocol version plus the
/// capabilities of the given direction.
pub fn own_table(extra: &[Cap]) -> Vec<Cap> {
    let mut t = vec![Cap { id: CAP_PROTOCOL_VERSION, payload: vec![PROTOCOL_VERSION] }];
    t.extend_from_slice(extra);
    t
}

pub fn find(caps: &[Cap], id: u8) -> Option<&Cap> {
    caps.iter().find(|c| c.id == id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_with_trailing_body() {
        let caps = vec![
            Cap { id: CAP_PROTOCOL_VERSION, payload: vec![1] },
            Cap { id: CAP_EXIT_STATUS, payload: vec![] },
        ];
        let mut bytes = encode_table(&caps);
        bytes.extend_from_slice(b"BODY");
        let (got, used) = decode_table(&bytes).unwrap();
        assert_eq!(got, caps);
        assert_eq!(&bytes[used..], b"BODY");
    }

    #[test]
    fn unknown_ids_are_preserved_and_skippable() {
        // A future peer's entry must parse by length and not disturb
        // anything after it.
        let caps = vec![
            Cap { id: 199, payload: vec![9, 9, 9] },
            Cap { id: CAP_EXIT_STATUS, payload: vec![7] },
        ];
        let (got, _) = decode_table(&encode_table(&caps)).unwrap();
        assert_eq!(find(&got, CAP_EXIT_STATUS).unwrap().payload, vec![7]);
        assert_eq!(find(&got, 199).unwrap().payload.len(), 3);
    }

    #[test]
    fn malformed_tables_reject() {
        assert!(decode_table(&[]).is_err()); // no count
        assert!(decode_table(&[1]).is_err()); // count without entry
        assert!(decode_table(&[1, 5, 4, 0, 0]).is_err()); // len 4, 2 bytes
        assert!(decode_table(&[2, 0, 0]).is_err()); // second entry missing
    }

    #[test]
    fn empty_table_is_one_byte() {
        let (caps, used) = decode_table(&encode_table(&[])).unwrap();
        assert!(caps.is_empty());
        assert_eq!(used, 1);
    }
}
```

**Step 2: Run** `just debug-cargo test -p posh caps::` — expect FAIL to compile until `mod.rs` registers the module, then PASS.

**Step 3: Commit** `feat: capability table encode/decode (RFC 0001 §3)`

---

### Task 2: Wire the table into `ClientMessage` and `ServerFrame`

**Promotion criteria:** baseline (bit-clear) decode path stays forever — it IS v0 compat.

**Files:**
- Modify: `crates/posh/src/remote/sync.rs`

**Step 1: Failing tests** (in `sync.rs` tests)

```rust
#[test]
fn client_message_caps_roundtrip_and_v0_compat() {
    // v0 bytes (no extension bit) decode to an empty table.
    let v0 = ClientMessage { flags: 0, caps: vec![], acked_frame: 1,
        rows: 24, cols: 80, input_base: 0, input: b"x".to_vec() };
    let enc = v0.encode();
    assert_eq!(enc[0] & caps::FLAG_EXTENSION, 0, "empty table must not set the bit");
    assert_eq!(ClientMessage::decode(&enc).unwrap(), v0);

    // v1: table rides behind the bit; the fixed fields and input survive.
    let v1 = ClientMessage { flags: CLIENT_FLAG_SHUTDOWN,
        caps: caps::own_table(&[caps::Cap { id: caps::CAP_EXIT_STATUS, payload: vec![] }]),
        acked_frame: 9, rows: 50, cols: 132, input_base: 7, input: b"hi".to_vec() };
    let enc = v1.encode();
    assert_ne!(enc[0] & caps::FLAG_EXTENSION, 0);
    assert_eq!(ClientMessage::decode(&enc).unwrap(), v1);
}

#[test]
fn server_frame_caps_roundtrip_and_v0_compat() {
    // mirror of the client test for ServerFrame (Full/Diff/Empty bodies
    // with and without a table).
}

#[test]
fn truncated_caps_reject_the_message() {
    let mut enc = ClientMessage { flags: 0,
        caps: vec![caps::Cap { id: 1, payload: vec![1, 2, 3] }],
        acked_frame: 0, rows: 1, cols: 1, input_base: 0, input: vec![] }.encode();
    enc.truncate(3); // cut inside the table
    assert!(ClientMessage::decode(&enc).is_err());
}
```

**Step 2: Implementation.** Both structs gain `pub caps: Vec<caps::Cap>`. Encode: write `flags | (if caps.is_empty() {0} else {FLAG_EXTENSION})`, then `encode_table` when non-empty, then the existing fixed fields. Decode: read flags at 0; if the bit is set, `decode_table(&data[1..])` and offset everything by `1 + used`, else offset 1 as today (the current hard-coded indices become `at`-relative). Strip `FLAG_EXTENSION` out of the `flags` value stored on the struct so `flags` keeps meaning runtime signals only.

All existing constructors set `caps: vec![]` (compile errors point at every site).

**Step 3:** `just debug-cargo test -p posh sync::` → PASS, then full `test --workspace` (the e2e suites exercise mixed encode/decode heavily).

**Step 4: Commit** `feat: datagram capability table behind the extension bit (RFC 0001 §3)`

---

### Task 3: `Target` parser (RFC 0001 §1)

**Files:**
- Create: `crates/posh/src/target.rs`
- Modify: `crates/posh/src/main.rs` (add `mod target;`)

**Step 1: Failing table-driven test** — encode the RFC's normative examples verbatim:

```rust
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    LocalSession { name: String },
    Local { group: Option<String>, session: String },
    Host { user: Option<String>, host: String },
    RemoteSession { user: Option<String>, host: String,
                    group: Option<String>, session: String },
}

#[cfg(test)]
mod tests {
    use super::Target::{self, *};
    fn s(v: &str) -> String { v.into() }

    #[test]
    fn rfc_normative_examples() {
        let cases: Vec<(&str, Target)> = vec![
            ("dev", LocalSession { name: s("dev") }),
            (":dev", Local { group: None, session: s("dev") }),
            (":grp/dev", Local { group: Some(s("grp")), session: s("dev") }),
            (":", LocalSession { name: s(":") }),
            ("box.example", Host { user: None, host: s("box.example") }),
            ("user@box", Host { user: Some(s("user")), host: s("box") }),
            ("box:dev", RemoteSession { user: None, host: s("box"), group: None, session: s("dev") }),
            ("user@box:grp/dev", RemoteSession { user: Some(s("user")), host: s("box"), group: Some(s("grp")), session: s("dev") }),
            ("[fe80::1]:dev", RemoteSession { user: None, host: s("fe80::1"), group: None, session: s("dev") }),
            ("fe80::1", Host { user: None, host: s("fe80::1") }),
            ("::1", Host { user: None, host: s("::1") }),
            ("box:", Host { user: None, host: s("box") }),
            ("[fe80::1]", Host { user: None, host: s("fe80::1") }),
            // group/ split requires both halves non-empty:
            ("box:/dev", RemoteSession { user: None, host: s("box"), group: None, session: s("/dev") }),
            ("box:grp/", RemoteSession { user: None, host: s("box"), group: None, session: s("grp/") }),
            // unterminated bracket falls through to rule 3 (':' inside
            // brackets makes the session part malformed -> Host):
            ("[fe80::1", Host { user: None, host: s("[fe80::1") }),
        ];
        for (input, want) in cases {
            assert_eq!(Target::parse(input), want, "input {input:?}");
        }
    }
}
```

**Step 2: Implementation** — rules in RFC order; total function; helpers `split_user` (first `@`) and `split_group` (first `/`, both halves non-empty). Bracket rule: `[`…`]` then optional `:suffix` (non-empty → RemoteSession). Rule 3: `split_once(':')`, session part valid iff non-empty and `!contains(':')`. Rule 6: `@`/`.` → Host, else LocalSession.

**Step 3:** test → PASS. **Step 4: Commit** `feat: total typed Target parser for the host:session grammar (RFC 0001 §1)`

---

### Task 4: Dispatch + HELP

**Files:**
- Modify: `crates/posh/src/main.rs` (bare-arg arm; delete `looks_like_ssh_destination` + its test — superseded by Target; HELP synopsis)

**Step 1:** Replace the bare-name arm:

```rust
name if !name.starts_with('-') => match target::Target::parse(name) {
    target::Target::LocalSession { .. } => cmd_attach(&group, rest),
    target::Target::Local { group: g, session } => {
        let g = g.unwrap_or(group);
        let args = once(session).chain(rest_after_first).collect();
        cmd_attach(&g, &args)
    }
    target::Target::Host { .. } => cmd_ssh(rest),
    target::Target::RemoteSession { .. } => cmd_ssh_session(parsed, rest),
},
```

(`cmd_ssh_session` lands in Task 5; for this task have it return a "not yet implemented" error so dispatch compiles, or sequence Tasks 4+5 in one commit if cleaner — implementer's call, both noted.)

HELP synopsis gains `posh [user@]host:[group/]session` and `posh :[group/]session`; the existing `@ . :` heuristic sentence is replaced by a pointer to the grammar ("scp-style; see the RFC").

**Step 2:** `help_covers_all_commands_and_env` still passes; add asserts for `host:` and `:group/session` strings.

**Step 3: Commit** `feat: bare-arg dispatch through Target (RFC 0001 §1)`

---

### Task 5: Remote-session attach (`cmd_ssh_session`)

**Files:**
- Modify: `crates/posh/src/remote/sshwrap.rs` (remote command builder + tests)
- Modify: `crates/posh/src/main.rs`

**Step 1: Failing test** in sshwrap: the composed remote command for `RemoteSession { host: "box", group: Some("grp"), session: "my dev" }` plus `-p 60100:60200`:

```
"LANG='…' posh-server new -p 60100:60200 -- 'posh' 'attach' '-g' 'grp' 'my dev'"
```

(reuses the existing `shell_quote` over every inner-argv element — the same lossless-argv discipline as #18.)

**Step 2:** Implement by building the inner argv `["posh", "attach", "-g", G, SESSION]` (omit `-g G` when group is None) and passing it through the existing `remote_command(command: &[String], …)` path; `cmd_ssh_session(t, flags)` = `cmd_ssh` with that argv as the `-- command…` and `t.user/host` as destination. ssh flags (`-4/-6/-p`) still parse from `rest` before the target (they were already consumed by cmd_ssh's parser; keep that).

**Step 3:** unit test PASS; full workspace suite.

**Step 4: Commit** `feat: posh host:session attaches over the roaming transport (RFC 0001 §2)`

---

### Task 6: EXIT_STATUS end-to-end

**Files:**
- Modify: `crates/posh/src/remote/client.rs` (advertise + consume)
- Modify: `crates/posh/src/remote/server.rs` (record peer caps; attach status on shutdown frames)
- Test: `crates/posh/tests/signal_integration.rs`

**Step 1: Failing e2e test** (pattern: existing remote tests; ports 62800–62899):

```rust
#[test]
fn remote_exit_status_propagates_over_udp() {
    // posh server new -- sh -c 'exit 7'; drive a client on a pty; when the
    // command exits the shutdown handshake must deliver 7 as the client's
    // process exit status.
}
```

And the composed variant (the headline): server command = `posh attach dev` against a daemon whose session runs `sh -c 'exit 7'` — asserts 7 end-to-end through BOTH layers (inner #18 path + the new capability).

**Step 2: Client side.** Every `ClientMessage` sends `caps::own_table(&[Cap { id: CAP_EXIT_STATUS, payload: vec![] }])`. On receiving a shutdown-flagged frame whose table has `CAP_EXIT_STATUS` with a 1-byte payload, store `st.exit_status = payload[0] as i32`; `client_loop` returns it; `cmd_ssh`/`cmd_client` exit the process with it (mirror of the session attach path from #18).

**Step 3: Server side.** Track `peer_wants_exit_status` (any received table containing CAP_EXIT_STATUS). The teardown already reaps via `util::try_reap`/`exit_code` — store the code at reap time; while winding down, shutdown frames carry `caps::own_table(&[Cap { id: CAP_EXIT_STATUS, payload: vec![code as u8] }])` **only when the peer advertised** (RFC MUST).

**Step 4:** e2e PASS; four-way skew is covered by Task 2's unit tests plus the existing e2e suite running v1↔v1.

**Step 5: Commit** `feat: session exit status over the roaming transport (closes the #18 remote gap)`

---

### Task 7: `posh list host:`

**Files:**
- Modify: `crates/posh/src/session/mod.rs` (`cmd_list`) or `main.rs` list arm
- Test: unit on the arg classification + command construction

**Step 1:** A `list` argument parsing as `Host` *with a trailing colon in the raw input* (e.g. `box:`) runs `ssh -o BatchMode=yes <host> posh list --short` and prints each line prefixed `host:`. Factor the ssh argv into a testable `remote_list_command(user, host) -> Vec<String>` and unit-test it; the spawn itself is a thin `std::process::Command` call.

**Step 2: Commit** `feat: posh list host: lists remote sessions (RFC 0001 §1)`

---

### Task 8: Remote-session completion

**Files:**
- Modify: `crates/posh/src/completions.rs` (bash + fish; zsh follows)

**Step 1: Failing structural tests:** scripts must reference `posh list` *through ssh* for `host:`-shaped current words, a cache under `${XDG_CACHE_HOME:-$HOME/.cache}/posh/`, `BatchMode=yes`, and `ConnectTimeout=2` (tuning levers per FDR).

**Step 2:** bash: when `cur` contains `:`, split host, complete from `_posh_remote_sessions <host>` (cached file ≤30s old, else `ssh -o BatchMode=yes -o ConnectTimeout=2 host posh list --short`), emitting `host:`-prefixed candidates. fish: same via a `__posh_remote_sessions` function and a `string match '*:*'` condition. `bash -n` gate already exists and must stay green.

**Step 3: Commit** `feat: complete remote session names for host: targets (#37)`

---

### Task 9: Docs + status promotion

**Files:**
- Modify: `README.md` (session-and-remote section: the namespace forms)
- Modify: `docs/manual-testing.md` (cross-host: `posh box:dev` flow, detach from one machine / reattach from another, exit-status check)
- Modify: `docs/rfcs/0001-target-grammar-and-capability-table.md` (status: proposed → accepted)
- Modify: `docs/features/0001-unified-host-session-namespace.md` (status: proposed → experimental, per its promotion criteria)

**Step 1:** Make the edits; re-read FDR promotion criteria to confirm the experimental gate is satisfied (it requires exactly this implementation + green conformance tests).

**Step 2: Commit** `docs: host:session namespace shipped — RFC accepted, FDR experimental`

---

## Execution notes

- Fast loop: `just debug-cargo test -p posh <filter>`; full gate runs in the merge hook — do NOT run `just` before merging.
- e2e tests allocate UDP ports: use the 62800–62899 range (62600–62699 and 62700–62799 are taken by existing suites).
- The merge flow is `spinclass merge-this-session(-async)` with git_sync after attestation — never `git push`.
- Tasks 4+5 may merge into one commit if the intermediate stub feels artificial; everything else lands one commit per task.
