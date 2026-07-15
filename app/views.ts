export function page(): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Your onvibe app</title>
  <style>
    * { box-sizing: border-box; }
    body { font-family: system-ui, sans-serif; max-width: 640px; margin: 64px auto; padding: 0 20px; color: #111; }
    h1 { font-size: 1.6rem; margin: 0 0 8px; }
    p.sub { color: #666; margin: 0 0 28px; }
    ul { list-style: none; padding: 0; margin: 0; display: grid; gap: 10px; }
    li { padding: 14px 16px; border: 1px solid #eee; border-radius: 10px; }
    li strong { display: block; margin-bottom: 2px; }
    li span { color: #666; font-size: .9rem; }
    code { background: #f4f4f5; padding: 1px 5px; border-radius: 4px; font-size: .9em; }
    .foot { margin-top: 28px; color: #999; font-size: .85rem; }
  </style>
</head>
<body>
  <h1>It's live 🎉</h1>
  <p class="sub">Your app is deployed and serving. Edit the code and it updates on the next deploy.</p>
  <ul>
    <li><strong>Server-side rendered</strong><span>This page is built in <code>main.ts</code> + <code>views.ts</code>.</span></li>
    <li><strong>No database yet</strong><span>Ask for a database when you need to store data — see the note in <code>main.ts</code>.</span></li>
    <li><strong>Errors are captured</strong><span>Frontend errors are reported automatically via <code>withErrorReporting</code>.</span></li>
  </ul>
  <p class="foot">Built with onvibe.run</p>
</body>
</html>`;
}
