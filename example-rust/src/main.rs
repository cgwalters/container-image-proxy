use anyhow::{anyhow, Result};
use gio::traits::{DBusProxyExt, SocketExt};
use std::os::unix::prelude::{FromRawFd, IntoRawFd};
use std::process::{Command, Stdio};

const OBJPATH: &str = "/container/image/proxy";
const IFACE: &str = "container.image.proxy";

fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    let image = args
        .get(0)
        .ok_or_else(|| anyhow!("Missing required image argument"))?;
    let (mysock, theirsock) = std::os::unix::net::UnixStream::pair()?;
    let theirsock = unsafe { std::fs::File::from_raw_fd(theirsock.into_raw_fd()) };
    let mut proc = Command::new("container-image-proxy");
    proc.arg("--sockfd=0")
        .arg(image)
        .stdin(Stdio::from(theirsock));
    let mut proc = proc.spawn()?;

    let cancellable = gio::NONE_CANCELLABLE;

    let mysock = unsafe { gio::Socket::from_fd(mysock.into_raw_fd())? };
    let mysock = mysock.connection_factory_create_connection();
    let conn = gio::DBusConnection::new_sync(
        &mysock,
        None,
        gio::DBusConnectionFlags::NONE,
        None,
        cancellable,
    )?;
    let proxyflags = gio::DBusProxyFlags::DO_NOT_LOAD_PROPERTIES;
    let proxy =
        gio::DBusProxy::new_sync(&conn, proxyflags, None, None, OBJPATH, IFACE, cancellable)?;
    let manifest_res = proxy.call_sync(
        "GetManifest",
        None,
        gio::DBusCallFlags::NONE,
        -1,
        cancellable,
    )?;
    println!("{:?}", manifest_res);

    proxy.call_sync("Shutdown", None, gio::DBusCallFlags::NONE, -1, cancellable)?;

    let r = proc.wait()?;
    if !r.success() {
        return Err(anyhow!("proxy exited with error: {}", r));
    }

    Ok(())
}
