// Version flow-out: version.env (POSH_VERSION) is the repo's single source of
// truth (per eng-versioning(7)). This build script resolves it and *flows* it
// into the crate as a compile-time env var, so runtime code reads
// env!("POSH_VERSION") rather than CARGO_PKG_VERSION. Cargo's package.version
// stays an inert "0.0.0" placeholder (see the root Cargo.toml) that nothing
// reads for the actual version — so there is nothing to keep in sync and no
// drift to guard against.
//
// posh-term exposes the flowed value through posh_term::version(), which the
// posh-rec recorder stamps into the .castx `emu_rev` header so golden frames
// can be audited against the emulator revision that produced them.
//
// The authoritative version resolves in order:
//   1. $POSH_VERSION in the build environment (set by the nix derivation).
//   2. ../../version.env relative to the crate (dev builds from the workspace
//      checkout; this crate is at crates/posh-term/).
//   3. CARGO_PKG_VERSION as a never-hit fallback (only when neither source
//      exists, e.g. a published crate tarball), so env!("POSH_VERSION") always
//      resolves.

use std::env;
use std::fs;
use std::path::PathBuf;

fn main() {
    println!("cargo:rerun-if-env-changed=POSH_VERSION");

    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let version_env = manifest_dir.join("../../version.env");
    println!("cargo:rerun-if-changed={}", version_env.display());

    let version = env::var("POSH_VERSION")
        .ok()
        .or_else(|| {
            fs::read_to_string(&version_env)
                .ok()
                .as_deref()
                .and_then(parse_posh_version)
        })
        .or_else(|| env::var("CARGO_PKG_VERSION").ok())
        .expect("no version source: POSH_VERSION, version.env, or CARGO_PKG_VERSION");

    // Flow the authoritative version into the crate. Runtime: env!("POSH_VERSION").
    println!("cargo:rustc-env=POSH_VERSION={version}");
}

// Hand-rolled parse (no regex crate dependency in the build script):
// the first non-comment line whose key — with an optional `export `
// prefix — is POSH_VERSION, with surrounding whitespace and optional
// quotes stripped.
fn parse_posh_version(content: &str) -> Option<String> {
    for line in content.lines() {
        let trimmed = line.trim_start();
        if trimmed.starts_with('#') || trimmed.is_empty() {
            continue;
        }
        let body = trimmed.strip_prefix("export ").unwrap_or(trimmed);
        let (key, value) = body.split_once('=')?;
        if key.trim() != "POSH_VERSION" {
            continue;
        }
        let value = value.trim().trim_matches(|c| c == '"' || c == '\'').to_string();
        if value.is_empty() {
            return None;
        }
        return Some(value);
    }
    None
}
