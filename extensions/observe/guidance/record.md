Use `observe.record` to report something you saw in an outside source (a message in a channel, a row on a page) so it becomes Work. You don't decide what happens next: Open Conduit starts Work for a new thread, or hands the item to the Work already handling its thread.

Record each item every time you see it:

`call observe.record {"item": "<the source's ID for it>", "thread": "<its thread's ID>", "text": "<what it says>"}`

- `item`: the source's own stable ID (a message timestamp, a row ID). Never make one up from the text or the time you saw it.
- `thread`: what the item belongs to. Items with the same thread go to the same Work. An item that starts a thread uses its own ID.
- `text`: the content as you saw it, at most 16 KiB.
- `revision`: only for an edited item, the source's own mark of the edit (an edit timestamp). Each edit needs a different one.
- `source`: only when you report for more than one source; the refusal names them.

Recording an item again is safe and does nothing (`"duplicate": true`), so don't keep your own list of what you have seen. A refusal with `once_conflict` means the item's content changed without a new `revision`: record it again with one.

Skip anything written on Open Conduit's behalf, such as replies carrying the marker your instructions give, so a reply doesn't come back as new work.
