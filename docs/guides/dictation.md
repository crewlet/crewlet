# Voice Dictation

The engine transcribes speech for the dashboard's composers itself. There is nothing to set up: no API key, no vendor account and nothing to install. Audio never leaves the engine, and nothing a person says is stored or logged.

## How it works

1. The browser captures the microphone, converts it to 16 kHz mono PCM and streams it over [`WS /ws/dictation`](../reference/api-endpoints.md#ws-wsdictation) to the engine it is signed in to.
2. The engine's recognizer answers with provisional words about every 0.3 s while the person speaks, and with a punctuated, cased segment when they pause (about 1.2 s after the last word).
3. The words land in the composer. The person sends them through the composer's own write; the dictation socket writes nothing.

The recognizer is k2-fsa's [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) with a streaming Zipformer model trained on LibriSpeech and GigaSpeech, plus an English punctuation and casing model. It recognises English.

- **It runs in a child process**, `crewlet speech-worker`, which the engine starts from its own binary. A crash in the recognizer's native code ends the dictations it was serving, which are told so, and the next dictation starts a new worker. The engine itself is unaffected, and the exit is logged as `speech_worker_exited`.
- **It loads on the first dictation** (about two seconds once the model is on disk) and exits after ten minutes with no dictation, handing its memory back.
- **One worker serves every dictation on the node**, sharing one loaded model.

## Privacy

- Audio and text exist only in the engine's memory, for the life of one dictation. They are never written to disk, a log, an event or a metric.
- Each dictation writes one log line, `dictation_session_ended`, carrying the person's id, the duration, the audio's size, how it ended and an error code. It never carries a word.
- The socket is admitted and kept open by the same rules as the dashboard's live socket: the signed-in session or bearer, a person who may act, and an `Origin` this deployment is reached at. When the session ends or is revoked, an open dictation closes with `4401`.

## What it costs

Measured on a cloud EPYC vCPU with 11 English clips:

| | |
|---|---|
| Accuracy | 2.3% word error rate on clean speech; real microphones, accents and noise raise it |
| Latency | first words about 0.6 s after speech starts; a new partial every 0.3 s |
| CPU | about 0.2 of a core per live dictation |
| Memory | about 320 MB while the worker is loaded, plus 10 to 20 MB per live dictation |
| Disk | the model, 188 MB; the recognizer library, 31 to 40 MB depending on the platform |
| First download | 229 MB, once, when nothing is prepared: measured at 11 s on a datacenter link, and 12 s to the first words of a first dictation |

## Where the recognizer comes from

Nothing of the recognizer is in the binary. It is two pieces, each pinned in the engine (`internal/speech`) with the SHA-256 of every file it loads:

- **the library**: the official sherpa-onnx release archive for this platform (9.8 to 12.8 MB), of which the C API and onnxruntime are kept;
- **the model**: its four int8 files from the model's Hugging Face repository at a pinned commit (189 MB), and the punctuation model's two files from its sherpa-onnx release archive (30.7 MB, which upstream publishes no other way).

The engine looks for them in this order, and verifies them wherever it finds them:

1. `dictation.recognizer_dir`, when set: a directory written by `crewlet speech prepare`. The engine only reads it; a copy that does not verify makes dictation unavailable with the reason `model_unavailable` rather than downloading.
2. Beside the binary, in `<prefix>/share/crewlet/speech`. The container image bakes both there (`/usr/local/share/crewlet/speech`), so a pod needs no egress for dictation.
3. Otherwise the first dictation downloads what is missing, once, while the person is shown its progress: the library into the native-library cache (`$TURSO_GO_CACHE_DIR/sherpa-onnx/`), the model into the data directory beside `store.path` (`<dir>/speech/`).

**For a host with no route to GitHub and Hugging Face,** prepare the recognizer on a connected machine and hand it over:

```sh
crewlet speech prepare -dir /srv/crewlet-speech -platform linux/amd64   # on a connected machine
# copy /srv/crewlet-speech to the host, then in crewlet.yaml:
#   dictation:
#     recognizer_dir: /srv/crewlet-speech
```

`-platform` defaults to the machine `prepare` runs on; the libraries are pinned for `linux/amd64`, `linux/arm64`, `darwin/amd64` and `darwin/arm64`.

## Configuration

Dictation is Tier A, because what it sizes is the node's CPU, memory and disk. Every field is optional:

```yaml
dictation:
  enabled: true          # unset = true
  recognizer_dir: ""     # a prepared recognizer; empty uses the image's or downloads
  max_concurrent: 0      # 0 = 4 live dictations per available core
  session_seconds: 300   # the longest dictation, by time and by audio
  idle_seconds: 15       # ends a dictation that sends no audio
```

See [Tier A](../getting-started/configuration.md#tier-a) for where the file lives.

## On Kubernetes

- The published image carries the library and the model, so nothing is downloaded, and a read-only root filesystem needs nothing written.
- An image built without them downloads the library into `TURSO_GO_CACHE_DIR`, which the operator already points at the data volume, and the model beside the store, on the same volume.
- Allow about 400 MB more memory for a node whose people dictate.

## Limits and what a person sees

| Limit | Default | What a person sees past it |
|---|---|---|
| Dictations per person per node | 1 | `busy` (`session`): the dictation open in another tab must end first |
| Live dictations per node | 4 per available core | `busy` (`capacity`): the dashboard can use the browser's own recognition instead |
| Length | 5 minutes of time or audio | the dictation ends `length`, keeping what was said |
| Silence | 15 s without audio | the dictation ends `idle`, keeping what was said |
| Audio sent before the recognizer is ready | 10 s | the dictation ends `preparing`; it can start again once the model is ready |

Whether dictation is offered at all is on the `viewer` answer as `dictation: {available, reason}`. The reasons:

| Reason | Meaning |
|---|---|
| `disabled` | `dictation.enabled: false` on this node |
| `unsupported_platform` | a build with no recognizer library (the four release platforms all have one) |
| `model_unavailable` | the library or the model could not be read, verified or downloaded; tried again a minute after the failure |
| `failed` | the recognizer would not start; tried again a minute after the failure |

## Troubleshooting

The log lines, all at `info` unless noted:

| Line | Says |
|---|---|
| `speech_library_fetching`, `speech_library_fetched` | the library is being downloaded into the native-library cache, and is there |
| `speech_model_fetching`, `speech_model_fetched` | the model is being downloaded into the data directory, and is there |
| `speech_worker_started` | a worker loaded the model, with how long that took |
| `speech_prepare_failed` (`error`) | the library, the model or the worker could not be readied: the reason and the cause |
| `speech_worker_exited` (`error`) | a worker ended on its own, with its exit status and how many dictations it was serving |
| `speech_worker_unloaded` | an idle worker was stopped |
| `dictation_session_ended` | one dictation ended: person, duration, audio size, reason, error code |
| `dictation_refused` | a dictation was told `busy` |
