# Try Promptape in 2 minutes (Python + Claude)

A tiny app with two AI-powered functions and two tests. You'll run the tests
twice: the first run calls Claude for real, the second is free and instant.

You need Python 3.9+, an Anthropic API key, and Promptape
([download](https://github.com/promptape/promptape/releases/latest) or
`go install github.com/promptape/promptape/cmd/promptape@latest`).

## 1. Start Promptape (terminal 1)

```bash
cd examples/python
promptape
```

Leave it running. It prints a line for every call.

## 2. Run the tests (terminal 2)

macOS / Linux:

```bash
cd examples/python
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

export ANTHROPIC_BASE_URL=http://127.0.0.1:4000
export ANTHROPIC_API_KEY=sk-ant-...   # your real key, only used while recording

pytest -v    # first run: real calls, recorded into ./tapes
pytest -v    # second run: replayed from tape, $0
```

Windows (PowerShell):

```powershell
cd examples/python
python -m venv .venv; .venv\Scripts\Activate.ps1
pip install -r requirements.txt

$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:4000"
$env:ANTHROPIC_API_KEY = "sk-ant-..."

pytest -v
pytest -v
```

## 3. Look at what happened

In terminal 1 you'll see `RECORD` lines with what each call cost on the first
run, then `REPLAY` lines with what each one saved. Then:

```bash
promptape report          # what a live run of these tests costs
ls tapes/anthropic        # the recordings: readable JSON, no API key inside
```

## 4. Prove it works offline

Stop Promptape (Ctrl+C), start it in replay mode, and run the tests with a
fake key and no internet:

```bash
promptape --mode replay                      # terminal 1
ANTHROPIC_API_KEY=not-a-real-key pytest -v   # terminal 2
```

They still pass, because nothing reaches Anthropic.
