# demo.tape — VHS tape (https://github.com/charmbracelet/vhs) recording the
# pod-triage hero for the README GIF (images/demo.gif).
#
# Regenerate with: make demo   (runs hack/gen-demo-gif.sh, which seeds
#   samples/fixtures/failing-pods.yaml — crashy=OOMKilled, web-broken=ImagePullBackOff,
#   healthy-web — waits for the crash-loop, records this tape, then removes it).
#
# Prereqs: vhs (brew install vhs), a throwaway cluster with an empty default ns
#   (e.g. kind create cluster), a local model server, and LLAMACPP_HOST exported.
#   The README transcript was captured with gemma3:4b — keep the model here in sync.

Output images/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1200
Set Height 600
Set Theme "Dracula"
Set Padding 20
Set TypingSpeed 30ms

Type "LLAMACPP_HOST=http://127.0.0.1:11434/ ottoflow run pod-triage --workflow-dir samples -n ottoflow --model gemma3:4b"
Enter

# collectPods (~25ms) + the agent call (~1s on gemma3:4b) + publishTriage.
Sleep 5s

# Hold on the final verdict before cutting.
Sleep 2s
