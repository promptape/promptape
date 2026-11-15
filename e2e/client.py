"""Exercises Promptape through the official OpenAI and Anthropic SDKs.

Prints a JSON document of everything the SDKs returned so two runs (one
recording, one replaying) can be compared byte for byte.
"""

import json
import os

import anthropic
import openai

BASE = os.environ["PROMPTAPE_URL"]

oai = openai.OpenAI(base_url=BASE + "/v1", api_key="sk-proj-e2e-dummy-key-not-real-000000", max_retries=0)
ant = anthropic.Anthropic(base_url=BASE, api_key="sk-ant-e2e-dummy-key-not-real-000000", max_retries=0)

out = {}

r = oai.chat.completions.create(
    model="gpt-5", messages=[{"role": "user", "content": "Summarise the plot of Hamlet."}]
)
out["openai_chat"] = {"id": r.id, "text": r.choices[0].message.content, "usage": r.usage.total_tokens}

stream = oai.chat.completions.create(
    model="gpt-5-mini",
    messages=[{"role": "user", "content": "Write a haiku about tests."}],
    stream=True,
    stream_options={"include_usage": True},
)
parts, usage = [], None
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        parts.append(chunk.choices[0].delta.content)
    if chunk.usage:
        usage = chunk.usage.total_tokens
out["openai_stream"] = {"text": "".join(parts), "usage": usage}

e = oai.embeddings.create(model="text-embedding-3-small", input="hello")
out["openai_embedding"] = e.data[0].embedding

m = ant.messages.create(
    model="claude-opus-5-5", max_tokens=1024,
    messages=[{"role": "user", "content": "Explain record/replay testing in one line."}],
)
out["anthropic_message"] = {"id": m.id, "text": m.content[0].text, "out": m.usage.output_tokens}

with ant.messages.stream(
    model="claude-sonnet-5-5", max_tokens=1024,
    messages=[{"role": "user", "content": "Name three uses of a cassette tape."}],
) as s:
    deltas = [t for t in s.text_stream]
    final = s.get_final_message()
out["anthropic_stream"] = {
    "deltas": len(deltas),
    "text": final.content[0].text,
    "stop": final.stop_reason,
    "out": final.usage.output_tokens,
}

# Identical request twice: each call gets its own recorded response, in order.
repeat = [
    ant.messages.create(model="claude-haiku-5-5", max_tokens=64,
                        messages=[{"role": "user", "content": "Roll a die."}]).content[0].text
    for _ in range(2)
]
out["anthropic_repeat"] = repeat

print(json.dumps(out, indent=2, sort_keys=True))
