# Where files you produce must go

**Every file the user is meant to receive goes in `public/attachments/`.**
Not the workspace root, not `/tmp`, not a folder you invent, not beside the
script that made it. There is one delivery folder and this is it.

This is not a preference. `public/` is the only directory the user's interface
shows, and `public/attachments/` is the folder it lists files from. A file
written anywhere else does not exist as far as the user is concerned — they
cannot see it, click it or download it. You will have done the work and delivered
nothing.

## The rule

- Producing a report, export, chart, archive, dataset, converted file, or
  anything else the user asked for → write it to `public/attachments/<name>`.
- Working files you need only for yourself → anywhere else you like. If the user
  never has to open it, it does not belong in `public/`.
- Unsure whether the user wants the file? Put it in `public/attachments/`. A
  visible file they ignore costs nothing; an invisible one they wanted costs them
  the whole request.

## Always say where you put it

When you deliver a file, **write its path in your reply**, in your own words:

> Salvei o relatório em `public/attachments/relatorio-q2.pdf`.

Do this even though the interface also renders a download chip. The chip is
added by the layer between you and the user and is **not part of the message
that gets saved** — so on a page reload it is gone, and a reply that said only
"file delivered" becomes a message about a file with no way to find it. Your own
sentence is the part that persists. Name the file and name the folder.

Never announce a file you did not actually write, and never announce a path you
did not actually use.

## `uploads/` is the old name

Older conversations and older references say `uploads/` where this says
`public/`. The two are the same directory: it was renamed, and existing files
moved with it. Reading an old `uploads/...` path still works. **Write new files
to `public/attachments/` only** — do not create an `uploads/` folder to match an
old reference.
