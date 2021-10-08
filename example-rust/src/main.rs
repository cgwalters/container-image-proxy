use anyhow::{anyhow, Context, Result};
use nix::sys::socket as nixsocket;
use nix::sys::uio::IoVec;
use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io::Read;
use std::os::unix::io::AsRawFd;
use std::os::unix::prelude::{FromRawFd, RawFd};
use std::process::{Child, Command, Stdio};

#[derive(Serialize)]
struct Request {
    method: String,
    args: Vec<serde_json::Value>,
}

#[derive(Deserialize)]
struct Reply {
    success: bool,
    error: String,
    pipeid: u32,
    value: serde_json::Value,
}

struct Proxy {
    sockfd: File,
    proc: Child,
}

struct GetManifestReply {
    digest: String,
    manifest: Vec<u8>,
}

impl Proxy {
    fn new(sockfd: File, proc: Child) -> Self {
        Self { sockfd, proc }
    }

    fn send_request(&mut self, req: Request) -> Result<()> {
        let buf = serde_json::to_vec(&req)?;
        nixsocket::send(self.sockfd.as_raw_fd(), &buf, nixsocket::MsgFlags::empty())?;
        Ok(())
    }

    fn get_reply<T: serde::de::DeserializeOwned>(&mut self) -> Result<(T, Option<(File, u32)>)> {
        let mut buf = [0u8; 16 * 1024];
        let mut cmsg_buffer = nix::cmsg_space!([RawFd; 1]);
        let iov = IoVec::from_mut_slice(buf.as_mut());
        let r = nixsocket::recvmsg(
            self.sockfd.as_raw_fd(),
            &[iov],
            Some(&mut cmsg_buffer),
            nixsocket::MsgFlags::MSG_CMSG_CLOEXEC,
        )?;
        let buf = &buf[0..r.bytes];
        let mut fdret: Option<File> = None;
        for cmsg in r.cmsgs() {
            if let nixsocket::ControlMessageOwned::ScmRights(fds) = cmsg {
                if let Some(&fd) = fds.get(0) {
                    let fd = unsafe { std::fs::File::from_raw_fd(fd) };
                    fdret = Some(fd);
                }
                break;
            }
        }
        let reply: Reply = serde_json::from_slice(buf).context("Deserializing reply")?;
        if !reply.success {
            return Err(anyhow!("remote error: {}", reply.error));
        }
        let fdret = match (fdret, reply.pipeid) {
            (Some(fd), n) => {
                if n == 0 {
                    return Err(anyhow!("got fd but no pipeid"));
                }
                Some((fd, n))
            }
            (None, n) => {
                if n != 0 {
                    return Err(anyhow!("got no fd with pipeid {}", n));
                }
                None
            }
        };
        let reply = serde_json::from_value(reply.value).context("Deserializing value")?;
        Ok((reply, fdret))
    }

    fn get_manifest(&mut self) -> Result<GetManifestReply> {
        let req = Request {
            method: "GetManifest".to_string(),
            args: vec![],
        };
        self.send_request(req)?;
        let (digest, fd) = self.get_reply::<String>()?;
        let (fd, pipeid) = fd.ok_or_else(|| anyhow!("Missing fd from reply"))?;
        // TODO make this async
        let reader = std::thread::spawn(move || -> Result<_> {
            let mut fd = std::io::BufReader::new(fd);
            let mut manifest = Vec::new();
            fd.read_to_end(&mut manifest)?;
            Ok(manifest)
        });
        self.finish_pipe(pipeid)?;
        let manifest = reader.join().unwrap()?;
        Ok(GetManifestReply { digest, manifest })
    }

    fn finish_pipe(&mut self, pipeid: u32) -> Result<()> {
        let req = Request {
            method: "FinishPipe".to_string(),
            args: vec![pipeid.into()],
        };
        self.send_request(req)?;
        let (r, fd) = self.get_reply::<()>()?;
        if fd.is_some() {
            return Err(anyhow!("Unexpected fd in finish_pipe reply"));
        }
        Ok(r)
    }

    fn shutdown(mut self) -> Result<()> {
        self.send_request(Request {
            method: "Shutdown".to_string(),
            args: vec![],
        })?;
        let r = self.proc.wait()?;
        if !r.success() {
            return Err(anyhow!("proxy exited with error: {}", r));
        }
        Ok(())
    }
}

fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    let image = args
        .get(1)
        .ok_or_else(|| anyhow!("Missing required image argument"))?;
    let (mysock, theirsock) = nixsocket::socketpair(
        nixsocket::AddressFamily::Unix,
        nixsocket::SockType::SeqPacket,
        None,
        nixsocket::SockFlag::SOCK_CLOEXEC,
    )?;
    // Convert to owned values
    let mysock = unsafe { std::fs::File::from_raw_fd(mysock) };
    let theirsock = unsafe { std::fs::File::from_raw_fd(theirsock) };
    let mut proc = Command::new("container-image-proxy");
    proc.arg("--sockfd=0")
        .arg(image)
        .stdin(Stdio::from(theirsock));
    let proc = proc.spawn()?;

    let mut proxy = Proxy::new(mysock, proc);

    let r = proxy.get_manifest()?;
    println!("digest: {:?} ({} bytes)", r.digest, r.manifest.len());

    proxy.shutdown()?;

    Ok(())
}
