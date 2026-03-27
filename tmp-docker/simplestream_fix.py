#!/usr/bin/env python3
"""PR #1107: Fix dangling pointer in simplestream sendJSON length prefix."""
import sys

path = sys.argv[1]
with open(path) as f:
    content = f.read()

# In all three functions (audio_stream, call_start, call_end), move json_length
# declaration outside the if(sendJSON) block so it's alive at send time.
# Pattern: add "uint32_t json_length = 0;" before "if (stream.sendJSON==true){"
# and change "uint32_t json_length = json_string.length();" to "json_length = ..."

old = 'std::vector<boost::asio::const_buffer> send_buffer;\n            if (stream.sendJSON==true){'
new = 'std::vector<boost::asio::const_buffer> send_buffer;\n            uint32_t json_length = 0;\n            if (stream.sendJSON==true){'
content = content.replace(old, new, 1)

old = 'std::vector<boost::asio::const_buffer> send_buffer;\n              if (stream.sendJSON==true){'
new = 'std::vector<boost::asio::const_buffer> send_buffer;\n              uint32_t json_length = 0;\n              if (stream.sendJSON==true){'
content = content.replace(old, new)  # two occurrences (call_start, call_end)

content = content.replace(
    'uint32_t json_length = json_string.length();',
    'json_length = json_string.length();'
)

with open(path, 'w') as f:
    f.write(content)

print('simplestream_fix.py: patched', path)
