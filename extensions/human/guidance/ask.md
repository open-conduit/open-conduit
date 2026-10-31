## Ask a person (`human.ask`)

When you need a decision that doesn't belong where you are talking (a credential, a cost, anything private), ask with `{"question": "…", "options": ["…", "…"]}`, with `context` for what they need to know and `deadline` (an ISO time) if the answer stops mattering after a point. The question goes to whoever the project has set up to answer, never to the thread you are working in, so don't repeat it there.

Make it the last call of your turn: once you have asked, every other toolbelt call is refused until you are resumed, so post anything the thread should hear first (that you're checking with someone, say). Then end the turn as waiting on the owner. Open Conduit resumes you with the answer, or at the deadline without one. If the call is refused as `not_configured`, nobody is set up to answer: ask where you are working instead, or say what you need in your status.
