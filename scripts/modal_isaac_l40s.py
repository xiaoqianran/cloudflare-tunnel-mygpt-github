#!/usr/bin/env python3
"""Cost-conscious NVIDIA Isaac Sim 5.1 smoke tests on a Modal L40S Sandbox.

Run with the Modal CLI so this file uses Modal's own uv-managed Python env:

    modal run scripts/modal_isaac_l40s.py --stage control
    modal run scripts/modal_isaac_l40s.py --stage compat
    modal run scripts/modal_isaac_l40s.py --stage render
    modal run scripts/modal_isaac_l40s.py --stage all

Stages are deliberately gated:
- control: tiny Ubuntu image; proves L40S allocation/capabilities only.
- compat: NVIDIA's official Isaac Sim 5.1 compatibility checker.
- render: NVIDIA's bundled Replicator standalone example; requires a newly
  produced PNG, not merely a zero exit code.
- all: compat -> render in ONE L40S Sandbox to avoid paying for a second GPU
  startup and to reuse the same container-local caches.

The script intentionally does NOT install Mesa Vulkan packages and does NOT set
NVIDIA_DRIVER_CAPABILITIES. Both would make the result easier to misread.
"""

from __future__ import annotations

import modal

APP_NAME = "isaac-sim-l40s-smoke"
ISAAC_TAG = "nvcr.io/nvidia/isaac-sim:5.1.0"
GPU = "L40S"

app = modal.App(APP_NAME)

# Keep the control image tiny. It is only a Modal/L40S control-plane check.
control_image = modal.Image.from_registry("ubuntu:22.04")

# Use NVIDIA's image as-is. Do not add Python, Mesa, Vulkan ICDs, X11, etc.
# Sandbox.create executes commands directly in the image, so Modal does not need
# to inject a Python runtime into the Isaac container.
isaac_image = modal.Image.from_registry(ISAAC_TAG)


def _read_streams(sb: modal.Sandbox) -> tuple[str, str, int]:
    stdout = sb.stdout.read()
    stderr = sb.stderr.read()
    sb.wait()
    rc = sb.returncode
    if rc is None:
        raise RuntimeError("Modal Sandbox completed without a return code")
    return stdout, stderr, rc


def _run_sandbox(*, image: modal.Image, command: str, cpu: float, memory: int, timeout: int) -> None:
    sb = modal.Sandbox.create(
        "bash",
        "-lc",
        command,
        app=app,
        image=image,
        gpu=GPU,
        cpu=cpu,
        memory=memory,
        timeout=timeout,
        idle_timeout=min(timeout, 180),
        workdir="/isaac-sim" if image is isaac_image else "/",
        # ACCEPT_EULA is required by NVIDIA's Isaac Sim container. We leave
        # PRIVACY_CONSENT unset (NVIDIA documents that as opt-out).
        env={"ACCEPT_EULA": "Y"} if image is isaac_image else None,
    )
    stdout, stderr, rc = _read_streams(sb)
    print(stdout, end="" if stdout.endswith("\n") else "\n")
    if stderr:
        print("--- STDERR ---")
        print(stderr, end="" if stderr.endswith("\n") else "\n")
    if rc != 0:
        raise SystemExit(f"Modal Sandbox failed with exit code {rc}")


CONTROL = r'''
set -euo pipefail
printf '%s\n' '=== CONTROL: Modal L40S Sandbox ==='
uname -m
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader
printf 'NVIDIA_DRIVER_CAPABILITIES=%s\n' "${NVIDIA_DRIVER_CAPABILITIES-<unset>}"
printf '%s\n' '--- devices ---'
ls -l /dev/nvidia* 2>&1 || true
printf '%s\n' '--- filesystem ---'
df -h / /tmp || true

name="$(nvidia-smi --query-gpu=name --format=csv,noheader | head -n1)"
driver="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -n1)"
case "$name" in
  *L40S*) ;;
  *) echo "[FAIL] expected L40S, got: $name"; exit 21 ;;
esac
printf '%s\n' "[PASS] L40S allocated: $name; driver=$driver"
'''


COMPAT = r'''
set -euo pipefail
printf '%s\n' '=== ISAAC 5.1 COMPATIBILITY CHECK ==='
printf 'image=%s\n' 'nvcr.io/nvidia/isaac-sim:5.1.0'
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader
printf 'NVIDIA_DRIVER_CAPABILITIES=%s\n' "${NVIDIA_DRIVER_CAPABILITIES-<unset>}"

test -x ./isaac-sim.compatibility_check.sh || {
  echo '[FAIL] official compatibility checker is missing from the NVIDIA image'
  exit 30
}

set +e
./isaac-sim.compatibility_check.sh --/app/quitAfter=10 --no-window 2>&1 | tee /tmp/isaac-compat.raw.log
checker_rc=${PIPESTATUS[0]}
set -e
# Remove terminal color/control sequences before machine-checking the result.
sed -E $'s/\x1B\[[0-9;?]*[ -\/]*[@-~]//g' /tmp/isaac-compat.raw.log > /tmp/isaac-compat.log

printf '\n=== COMPAT SUMMARY ===\n'
grep -Eai 'System checking result|GPU|driver|VRAM|Vulkan|Kit|error|fail|pass' /tmp/isaac-compat.log | tail -n 120 || true

if grep -Eaq 'System checking result:[[:space:]]*PASSED' /tmp/isaac-compat.log; then
  echo '[PASS] NVIDIA Isaac Sim compatibility checker'
else
  echo "[FAIL] compatibility checker did not report PASSED (rc=$checker_rc)"
  exit 31
fi
'''


RENDER = r'''
set -euo pipefail
printf '%s\n' '=== ISAAC 5.1 RTX/REPLICATOR RENDER ==='
example='./standalone_examples/api/isaacsim.replicator.examples/sdg_getting_started_01.py'
test -f "$example" || {
  echo "[FAIL] bundled NVIDIA Replicator example not found: $example"
  exit 40
}

rm -rf /tmp/isaac-render-smoke
mkdir -p /tmp/isaac-render-smoke
touch /tmp/isaac-render-smoke/.started
cd /tmp/isaac-render-smoke

set +e
/isaac-sim/python.sh "/isaac-sim/${example#./}" 2>&1 | tee /tmp/isaac-render.log
render_rc=${PIPESTATUS[0]}
set -e

printf '\n=== RENDER SUMMARY ===\n'
grep -Eai 'RTX|Vulkan|GPU|renderer|error|fail|fatal' /tmp/isaac-render.log | tail -n 160 || true

mapfile -t pngs < <(find /tmp/isaac-render-smoke /tmp -type f -name '*.png' -newer /tmp/isaac-render-smoke/.started -size +1024c 2>/dev/null | sort -u)
printf 'new_png_count=%d\n' "${#pngs[@]}"
printf '%s\n' "${pngs[@]:-}"

if (( render_rc != 0 )); then
  echo "[FAIL] Replicator example exited rc=$render_rc"
  exit 41
fi
if (( ${#pngs[@]} == 0 )); then
  echo '[FAIL] Replicator exited successfully but produced no new PNG > 1 KiB'
  exit 42
fi

first_png="${pngs[0]}"
file "$first_png" 2>/dev/null || true
sha256sum "$first_png" 2>/dev/null || true
echo "[PASS] Isaac Sim produced rendered PNG: $first_png"
'''


@app.local_entrypoint()
def main(stage: str = "control"):
    stage = stage.strip().lower()
    if stage not in {"control", "compat", "render", "all"}:
        raise SystemExit("stage must be one of: control, compat, render, all")

    if stage == "control":
        _run_sandbox(image=control_image, command=CONTROL, cpu=0.125, memory=128, timeout=45)
        return

    # NVIDIA's documented x86_64 minimum for Isaac Sim 5.1 is 4 cores / 32 GB
    # RAM / 16 GB VRAM. L40S has 48 GB VRAM. Giving the checker its documented
    # minimum avoids a false negative caused by intentionally tiny CPU/RAM.
    if stage == "compat":
        _run_sandbox(image=isaac_image, command=COMPAT, cpu=4.0, memory=32768, timeout=240)
        return

    if stage == "render":
        # Rendering itself does not need the compatibility checker's full RAM,
        # but keep enough headroom for Kit + RTX initialization.
        _run_sandbox(image=isaac_image, command=RENDER, cpu=4.0, memory=16384, timeout=300)
        return

    # Cheapest end-to-end form: one GPU Sandbox, one image startup, sequential
    # hard gates. `set -e` means render is never attempted if compat fails.
    all_command = COMPAT + "\n" + RENDER
    _run_sandbox(image=isaac_image, command=all_command, cpu=4.0, memory=32768, timeout=480)
