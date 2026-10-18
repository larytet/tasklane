# Release artifacts: signing and verification

A release is a directory of tarballs (`tasklane-VERSION-linux-amd64.tar.gz`, `…-arm64.tar.gz`), `SHA256SUMS` listing them, and `SHA256SUMS.sig`, an Ed25519 signature over the checksum file. Every archive carries the binary, `NOTICE` (licences), `SBOM.txt` (modules and their licences), the README, the configuration reference and the deployment files. The builds are reproducible: the same source and Go version give byte-identical archives, so anyone can rebuild and compare.

## Checking a release before installing it

```
tasklane verify-release release.pub DIR
```

`release.pub` is the publisher's public key, obtained **out of band** (not from the same download). The command needs no configuration, no database and no network, and exits 0 only when everything below holds:

1. **The signature** is valid for the checksum file under that key (the signed message is a fixed label and the file, so it cannot be replayed for anything else), and the key id in the signature file is the key you gave.
2. **Every archive** named in the signed list has the listed SHA-256, the names are plain file names (no paths), none is listed twice, and **no archive sits in the directory that the list does not name** (an unlisted archive is not part of the release).
3. **Each archive is what it says it is.** It has the binary, `NOTICE` and `SBOM.txt`; it has one top directory named for its version and architecture; it contains no links and no entry that leaves the directory; the binary's embedded build information says the architecture the name says and replaces no module; the **modules linked into the binary are exactly the modules in the SBOM, with the same versions, and each has an entry in the NOTICE**; and the version the release build wrote into the binary is the version in the archive's name.

Any problem is printed and the command exits 1: do not install that release. Exit 2 means the check could not be made (an unreadable key, no checksum file).

## Making a release

```
go run ./cmd/relsign keygen KEYDIR          # once: KEYDIR/release.key (secret) and release.pub (publish)
RELEASE_KEY=KEYDIR/release.key VERSION=1.4.0 make dist
```

`make dist` builds both archives and `SHA256SUMS`; with `RELEASE_KEY` set it also writes `SHA256SUMS.sig`. Ed25519 signatures are deterministic, so a rebuilt release has a byte-identical signature. Keep `release.key` off the build machines that run untrusted code; losing it means publishing a new public key out of band, and a leaked key cannot be revoked by anything in the product.

The signature format is standard: `SHA256SUMS.sig` is three lines (a header, `keyid:`, and the base64 signature) and the signature verifies with OpenSSL over the message `tasklane-release-v1` + a line feed + the checksum file (`openssl pkeyutl -verify -pubin -inkey release.pub -rawin -in message -sigfile sig.bin`); the test suite does exactly that to check the implementation against one that shares none of its code.

## Not covered

- **Key distribution and rotation.** There is no key in the repository and no keyring: an operator must get the public key from the publisher through a channel they trust, and a rotation is a new key announced the same way. Nothing in the product pins a key.
- **No transparency log, no signed build provenance (SLSA attestations), no per-file signatures inside an archive, and no signature on the container image**, only on the tarballs.
- **The version check reads a marker that the release build writes into the binary** (Go does not record linker flags in the build information), so it detects a binary built for another version; a hostile publisher who controls the build could write any marker, which is what the signature is for.
- The SBOM is the project's own simple list (module, version, licence), not SPDX or CycloneDX.
- Not built: the Helm chart and its checks (INST-01 on a cluster).
