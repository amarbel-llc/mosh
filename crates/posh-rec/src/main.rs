//! The `posh-rec` binary. Replay (`replay`, `version`, `help`) is handled by
//! the safe library ([`posh_rec::cli`]); `record` needs PTY/libc FFI and lives
//! in the bin-only [`record`] module so the library stays
//! `#![forbid(unsafe_code)]`. The replay surface is also reachable as
//! `posh rec ...` on the posh binary.

mod record;

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let result = match args.first().map(String::as_str) {
        Some("record") => record::run(&args[1..]),
        _ => posh_rec::cli::run(&args).map(|()| 0),
    };
    match result {
        Ok(code) => std::process::exit(code),
        Err(e) => {
            eprintln!("posh-rec: {e}");
            std::process::exit(1);
        }
    }
}
