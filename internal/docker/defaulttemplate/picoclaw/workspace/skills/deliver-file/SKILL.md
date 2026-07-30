---
name: deliver-file
description: Use when the user asks you to send, export, generate or share a FILE (report, spreadsheet, image, archive, PDF, dataset). Covers "me manda o arquivo", "gera um relatório", "exporta isso", "envia em PDF", "send me the file".
---

# Delivering a file to the user

The person you are talking to reads you through a web chat. They cannot see your
working directory, and a file you leave anywhere else is invisible to them.

**Write the file to `uploads/attachments/` and say so in your reply.**

`uploads/` is the folder this chat shares with the user: everything in it appears in
their Files panel, where they can click a file and download it. `attachments/` is
where your own deliverables go, so they are not mixed with what the user uploaded.

## Procedure

1. Create the directory if it is not there yet: `mkdir -p uploads/attachments`
2. Write the file into it, with a name a human would recognize —
   `uploads/attachments/vendas-q1-2026.xlsx`, not `uploads/attachments/out.bin`.
3. Reply in plain text naming the file **and** its path, for example:

   > Pronto — o relatório está em `uploads/attachments/vendas-q1-2026.xlsx`.
   > Abra o painel de Arquivos para baixar.

   Write that sentence in the user's own language.

## Rules

- **Never** claim you sent a file you did not write. If writing it failed, say what
  failed.
- Put the path in your reply **as text**. That text is part of the conversation, so
  it is still there when the user reloads the page — which is exactly when someone
  goes looking for a file again.
- Overwrite the same name for a corrected version instead of accumulating
  `report-final-v3.xlsx`. The user sees the folder.
- Do not base64 a file into the chat. Large blobs of text are unreadable and cost
  the user tokens; write the file and give the path.
- Keep large intermediates out of `uploads/` — use your own working directory and
  copy only the finished deliverable in.
