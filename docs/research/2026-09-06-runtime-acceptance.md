# Runtime acceptance, 2026-09-06

## Selected runtime

The independent Ubuntu image now uses WineHQ `11.0.0.0~jammy-1`, following
the Wine 10 reference qualification and Wine 11 comparison below. It follows
AndrewSav's verified sequence:
headless `winecfg`, five-second delay, Xvfb, then V Rising. The controller sets
`WINEDLLOVERRIDES=winhttp=n,b` for mods. No forced Mono/Gecko suppression is used.

Reference: [AndrewSav commit cc2e8e5](https://github.com/AndrewSav/vrising-docker/tree/cc2e8e5e4a2079e2567d954338479ec2a83d2126),
published image digest `sha256:20dfbbbc7a594d656371f85acda7690d5a49a35179ed7573af8ff861b47a8482`.
The published entrypoint matched the source byte-for-byte. Reference tests used
its actual entrypoint, `ENABLE_MODS=1`, `SKIP_UPDATE=1`, and all three mounts.

Wine 10 baseline image: `sha256:8fc2c29c17b42780a992cc56bb4e4730157dd599fc38c6e787015dd942205523`.

Wine 11 tested image: `sha256:9aafea70ba56e80e591f051cc7a7d60992db09f5455c257e63225b8932efa59a`.

## Verified behavior

- Published reference: vanilla, KindredCommands with dependencies, and the full
  Satisvampory stack reached startup completion. The full stack remained running
  for five minutes after readiness and wrote autosaves.
- Independent Wine 10 runtime: full-stack startup, persistent prefix, server
  working directory, controller HOME/display allocation, and existing-save load.
- Production updater: empty-volume Steam/mod installation and an offline restart
  passed; the offline start reported degraded updates while serving the same lock.
- Generated-save migration passed using disposable copies. Source files and
  existing settings were preserved; the game reported loading an existing save.
- Real Satisvampory `1.0.84` to `1.0.85` update passed. Both settings JSON hashes
  and an operator-owned mod config file remained unchanged.
- RCON authentication succeeded over the published loopback TCP port. UDP
  9876/9877 were bound. An additional A2S request timed out with listing disabled;
  player connectivity is a separate pending manual test.
- The update test ran with four CPUs and a 14-GiB memory limit. Tests must run
  sequentially: earlier overlapping game instances exhausted the 23-GiB host
  and one Kindred instance was OOM-killed. That run is not a stability pass.
- The final image restarted with updates disabled, remained healthy and
  non-degraded, and passed `vrisingctl verify` with no candidate or transaction.
- Source tests, vet, full race tests, shell helper tests, image contract, workflow
  lint, and all six container-fixture scenarios passed during implementation.

The verified current package lock is
`e2764ad1804bc8aa46d47f764f2c93f0a50c56d4f3ba1bc01c666479901598b7`:
BepInEx `1.733.2`, VCF `0.10.4`, KindredCommands `2.5.8`, HookDOTS API `1.1.1`,
and Satisvampory `1.0.85`, with Steam build `24686592`.

## Findings and limits

Actual VCF output is `VCF Loaded: 0.10.4`; the old synthetic marker was wrong.
Frozen startup previously reapplied the active generation and left a transaction
behind. It now verifies installed files without applying them. The resulting
false failed-generation marker in our disposable test data was archived and
cleared after stopping the container; no game save was restored.

One later cold start failed inside BepInEx interop generation with
`Cannot assign a 252 metadata token to a Param`, followed by missing
`Il2Cppmscorlib` and Wine's debugger. Earlier cold starts succeeded. The cause
of this intermittent failure is not established, and no speculative runtime
tuning was adopted. The captured generation error now fails startup promptly
instead of waiting for the readiness deadline. This is not a claim that the
underlying interop-generation problem is fixed.

## Wine 11 comparison and player confirmation

After the user connected on Wine 10 and confirmed `.whereami` and `.s help`,
the server completed `AutoSave_18.save.gz` after their disconnect. Wine 10 was
stopped and removed before the replacement was started. Its server and data
trees remain intact as the baseline.

The Wine 11 image was built from the same source with only the Wine package
version argument changed. Both images contain controller SHA-256
`83a06e17b9751ba3c64e0bbbb7f7d52038bfc0ddcb536bedb3be6e071aa8b6ee`.
The Wine 11 copy retained identical game, mod, configuration, admin-list, and
save inputs, with a fresh Wine prefix and updates disabled. The input save hash
was `9528025effd5950355661235a83ab2caeaaf187696f9cb465d229593baa0b032`.

Wine 11 passed the image contract, loaded the existing save and both mods,
passed deep installation verification and public-IP RCON authentication, and
remained healthy with zero container restarts. Server logs confirmed that the
user reconnected as the existing character Jediah; the user reported that the
test worked. Wine 11 is therefore the selected default.

This comparison reused the already-generated BepInEx interop assemblies. It
does not establish that Wine 11 fixes the intermittent cold-generation failure
described above. Fresh runtime acceptance remains a publication gate.

The user's original deployment save is not available on this host. Connection
testing used the generated world under `/opt/v-rising-test`, now containing the
user's test character. No image or commits have been published remotely.
