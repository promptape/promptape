from app import classify, summarise

TICKET = (
    "Hi, I was charged twice for my subscription this month. "
    "Order #4821. Can you refund the second payment? Thanks, Sam"
)


def test_summary_mentions_the_problem():
    summary = summarise(TICKET)
    assert summary
    assert any(word in summary.lower() for word in ("charge", "refund", "payment", "billing"))


def test_streamed_classification():
    assert "billing" in classify(TICKET)
