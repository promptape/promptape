"""A tiny "AI feature" to test: summarise a support ticket with Claude.

Nothing here knows about Promptape. The SDK reads ANTHROPIC_BASE_URL, so
pointing that at Promptape is all it takes.
"""

import anthropic

MODEL = "claude-opus-5-5"

client = anthropic.Anthropic()


def _text(message) -> str:
    if message.stop_reason == "refusal":
        raise RuntimeError("the model declined this request")
    # Responses can contain thinking blocks before the answer; keep the text.
    return "".join(block.text for block in message.content if block.type == "text").strip()


def summarise(ticket: str) -> str:
    """One-sentence summary of a support ticket."""
    message = client.messages.create(
        model=MODEL,
        max_tokens=1024,
        output_config={"effort": "low"},
        messages=[{
            "role": "user",
            "content": f"Summarise this support ticket in one sentence:\n\n{ticket}",
        }],
    )
    return _text(message)


def classify(ticket: str) -> str:
    """Streams the answer, like a chat UI would, and returns the label."""
    with client.messages.stream(
        model=MODEL,
        max_tokens=1024,
        output_config={"effort": "low"},
        messages=[{
            "role": "user",
            "content": "Classify this ticket as exactly one word: billing, bug, or other.\n\n" + ticket,
        }],
    ) as stream:
        final = stream.get_final_message()
    return _text(final).lower()
