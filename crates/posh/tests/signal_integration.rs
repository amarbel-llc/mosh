//! Signal-handling e2e for both client paths (github #14): SIGTERM must
//! wind the client down cleanly — restore the tty and exit 0 — instead of
//! dying with the default disposition (which leaves the user's shell in
//! raw mode and, on the remote path, the server lingering).

use std::os::fd::{FromRawFd, RawFd};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

fn posh_cmd() -> Command {
    Command::new(env!("CARGO_BIN_EXE_posh"))
}

/// posix_openpt master/slave pair; the slave becomes the child's stdio so
/// RawMode::enable and term_size see a real tty. The master is left
/// nonblocking for drain().
fn open_pty_pair() -> (RawFd, RawFd) {
    unsafe {
        let master = libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY);
        assert!(master >= 0, "posix_openpt failed");
        assert_eq!(libc::grantpt(master), 0, "grantpt failed");
        assert_eq!(libc::unlockpt(master), 0, "unlockpt failed");
        let mut name = [0 as libc::c_char; 128];
        assert_eq!(
            libc::ptsname_r(master, name.as_mut_ptr(), name.len()),
            0,
            "ptsname_r failed"
        );
        let slave = libc::open(name.as_ptr(), libc::O_RDWR | libc::O_NOCTTY);
        assert!(slave >= 0, "open pty slave failed");
        let flags = libc::fcntl(master, libc::F_GETFL);
        libc::fcntl(master, libc::F_SETFL, flags | libc::O_NONBLOCK);
        (master, slave)
    }
}

fn spawn_on_pty(cmd: &mut Command, slave: RawFd) -> Child {
    let stdio = |fd: RawFd| unsafe { Stdio::from_raw_fd(fd) };
    cmd.stdin(stdio(unsafe { libc::dup(slave) }))
        .stdout(stdio(unsafe { libc::dup(slave) }))
        .stderr(stdio(slave))
        .spawn()
        .expect("spawn posh on pty")
}

fn drain(master: RawFd) -> usize {
    let mut total = 0;
    let mut buf = [0u8; 4096];
    loop {
        let n = unsafe { libc::read(master, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n <= 0 {
            return total;
        }
        total += n as usize;
    }
}

fn wait_for_pty_output(master: RawFd, what: &str) {
    for _ in 0..400 {
        if drain(master) > 0 {
            return;
        }
        std::thread::sleep(Duration::from_millis(25));
    }
    panic!("timed out waiting for {what}");
}

/// Waits for exit while draining the pty so the child never blocks on a
/// full output buffer.
fn wait_for_exit(child: &mut Child, master: RawFd, secs: u64) -> std::process::ExitStatus {
    let deadline = Instant::now() + Duration::from_secs(secs);
    loop {
        drain(master);
        if let Some(status) = child.try_wait().expect("try_wait") {
            return status;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            panic!("client did not exit within {secs}s of SIGTERM");
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

#[test]
fn attach_client_exits_cleanly_on_sigterm() {
    let dir = std::env::temp_dir().join(format!("posh-sigtest-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();

    let out = posh_cmd()
        .args(["attach", "--detach", "sigtest", "sleep", "300"])
        .env("POSH_DIR", &dir)
        .env_remove("POSH_SESSION")
        .env_remove("POSH_GROUP")
        .output()
        .unwrap();
    assert!(out.status.success(), "attach --detach failed: {out:?}");

    let (master, slave) = open_pty_pair();
    let mut cmd = posh_cmd();
    cmd.args(["attach", "sigtest"])
        .env("POSH_DIR", &dir)
        .env_remove("POSH_SESSION")
        .env_remove("POSH_GROUP");
    let mut child = spawn_on_pty(&mut cmd, slave);
    wait_for_pty_output(master, "attach client first output");

    unsafe { libc::kill(child.id() as libc::pid_t, libc::SIGTERM) };
    let status = wait_for_exit(&mut child, master, 10);
    assert_eq!(
        status.code(),
        Some(0),
        "SIGTERM must detach cleanly (tty restore runs), got {status:?}"
    );

    let _ = posh_cmd()
        .args(["kill", "sigtest"])
        .env("POSH_DIR", &dir)
        .output();
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn remote_client_exits_cleanly_on_sigterm() {
    let out = posh_cmd()
        .args(["server", "-p", "62300:62399", "--", "sleep", "300"])
        .env("LC_ALL", "C.UTF-8")
        // Hygiene: if the shutdown handshake regresses, the detached
        // server still times itself out instead of lingering.
        .env("POSH_SERVER_NETWORK_TMOUT", "30")
        .output()
        .unwrap();
    assert!(out.status.success(), "server failed: {out:?}");
    let stdout = String::from_utf8_lossy(&out.stdout);
    let connect = stdout
        .lines()
        .find(|l| l.starts_with("POSH CONNECT "))
        .unwrap_or_else(|| panic!("no POSH CONNECT line in {stdout:?}"));
    let mut fields = connect.split_whitespace().skip(2);
    let port = fields.next().expect("port").to_string();
    let key = fields.next().expect("key").to_string();

    let (master, slave) = open_pty_pair();
    let mut cmd = posh_cmd();
    cmd.args(["client", "127.0.0.1", &port])
        .env("LC_ALL", "C.UTF-8")
        .env("POSH_KEY", key);
    let mut child = spawn_on_pty(&mut cmd, slave);
    wait_for_pty_output(master, "remote client first paint");

    unsafe { libc::kill(child.id() as libc::pid_t, libc::SIGTERM) };
    // The handler requests a server shutdown; the loop exits once the
    // server acks (well under the 5s grace period on loopback).
    let status = wait_for_exit(&mut child, master, 15);
    assert_eq!(
        status.code(),
        Some(0),
        "SIGTERM must wind down via the shutdown handshake, got {status:?}"
    );
}
