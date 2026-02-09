#!/usr/bin/env node
const http = require('http');

const PORT = process.env.PORT || 3456;

const server = http.createServer((req, res) => {
  const chunks = [];

  req.on('data', (chunk) => chunks.push(chunk));
  req.on('end', () => {
    const raw = Buffer.concat(chunks).toString('utf8');
    const timestamp = new Date().toISOString();

    console.log('\n' + '='.repeat(60));
    console.log(`[${timestamp}] ${req.method} ${req.url}`);
    console.log('Headers:', JSON.stringify(req.headers, null, 2));

    if (raw) {
      try {
        const parsed = JSON.parse(raw);
        console.log('Body (parsed JSON):');
        console.log(JSON.stringify(parsed, null, 2));
      } catch {
        console.log('Body (raw, not JSON):');
        console.log(raw);
      }
    } else {
      console.log('Body: (empty)');
    }
    console.log('='.repeat(60) + '\n');

    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true, received: true }));
  });
});

server.listen(PORT, () => {
  console.log(`Webhook listener: http://localhost:${PORT}`);
  console.log('Send POST requests to this URL to see the body logged as JSON.\n');
});
