# README demo: a staged replay that follows the behavior of a real recording
# of the healer on a k3d cluster; the kubectl output comes from realistic-env.sh.
# Re-record from the repo root:
#
#   docker run --rm -v "$PWD":/vhs -w /vhs ghcr.io/charmbracelet/vhs:v0.10.0 docs/demo/realistic.tape
#
# If VHS leaves the GIF empty (it sometimes does), build it from the MP4:
#
#   ffmpeg -i docs/demo/smb-mount-healer-realistic.mp4 -vf "fps=15,split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=none" docs/demo/smb-mount-healer-realistic.gif

Output docs/demo/smb-mount-healer-realistic.gif
Output docs/demo/smb-mount-healer-realistic.mp4

Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 720
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set WindowBar Colorful

Hide
Type "source docs/demo/realistic-env.sh" Enter
Sleep 300ms
Show

Type "# The NAS rebooted. The pod is still Running, but its SMB share is dead:" Sleep 400ms Enter
Type "kubectl -n media exec deploy/media-server -- ls /media" Sleep 300ms Enter
Sleep 2.5s

Type "# The NAS is back, but stale CIFS mounts don't recover on their own" Sleep 400ms Enter
Type "kubectl get smbmounts" Sleep 300ms Enter
Sleep 3.5s

Type "# Once the rechecks confirm Broken, the control plane remediates" Sleep 400ms Enter
Type "kubectl get smbr -w --watch-only" Sleep 300ms Enter
Sleep 26s
Ctrl+C
Sleep 800ms

Type "kubectl get smbmounts" Sleep 300ms Enter
Sleep 3s

Type "# Drained, unmounted, CSI driver restarted, scaled back up:" Sleep 400ms Enter
Type "kubectl -n media get pods" Sleep 300ms Enter
Sleep 1.5s
Type "kubectl -n media exec deploy/media-server -- ls /media" Sleep 300ms Enter
Sleep 4s
