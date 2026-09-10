# Third-party notices

The statically linked `unraid-vsock-sensors` executable incorporates the
components listed below. Each component remains subject to its own license.

| Component | Version | Copyright | License |
| --- | --- | --- | --- |
| Go standard library and runtime | Build-dependent | Copyright 2009 The Go Authors | [BSD 3-Clause](LICENSES/BSD-3-Clause-Go.txt) |
| `github.com/mdlayher/socket` | 0.6.0 | Copyright (C) 2021 Matt Layher | [MIT](LICENSES/MIT-mdlayher-socket.txt) |
| `github.com/mdlayher/vsock` | 1.3.0 | Copyright (C) 2017-2022 Matt Layher | [MIT](LICENSES/MIT-mdlayher-vsock.txt) |
| `golang.org/x/net` | 0.55.0 | Copyright 2009 The Go Authors | [BSD 3-Clause](LICENSES/BSD-3-Clause-Go.txt) |
| `golang.org/x/sync` | 0.20.0 | Copyright 2009 The Go Authors | [BSD 3-Clause](LICENSES/BSD-3-Clause-Go.txt) |
| `golang.org/x/sys` | 0.45.0 | Copyright 2009 The Go Authors | [BSD 3-Clause](LICENSES/BSD-3-Clause-Go.txt) |
| `gopkg.in/ini.v1` | 1.67.3 | Copyright 2014-2019 Unknwon and contributors | [Apache-2.0](LICENSES/Apache-2.0.txt) |

The Go standard library and runtime version is determined by the toolchain
used for each build. It can be inspected with `go version -m <binary>`.

No upstream `NOTICE` file is distributed by these module versions.
