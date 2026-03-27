#!/usr/bin/env python3
"""Move plugman_call_start to squelch-open (idle_count reset) instead of after conclude/restart."""
import sys

path = sys.argv[1]
with open(path) as f:
    content = f.read()

# 1. Add plugman_call_start after reset_idle_count
content = content.replace(
    '          // if it starts recording again, then reset the idle count\n'
    '          call->reset_idle_count();',
    '          // Squelch just opened after idle - this is when the call is actually active\n'
    '          call->reset_idle_count();\n'
    '          plugman_call_start(call);',
    1
)

# 2. Remove plugman_call_start from call_timeout path
content = content.replace(
    '          plugman_setup_recorder(recorder);\n'
    '          plugman_call_start(call);\n'
    '        }\n'
    '      } else if ((call->get_current_length()',
    '          plugman_setup_recorder(recorder);\n'
    '        }\n'
    '      } else if ((call->get_current_length()',
    1
)

# 3. Remove plugman_call_start from max_duration path
content = content.replace(
    '          plugman_setup_recorder(recorder);\n'
    '          plugman_call_start(call);\n'
    '        }\n'
    '      }\n'
    '    } else if (!call->get_recorder()->is_active())',
    '          plugman_setup_recorder(recorder);\n'
    '        }\n'
    '      }\n'
    '    } else if (!call->get_recorder()->is_active())',
    1
)

with open(path, 'w') as f:
    f.write(content)

print('call_start_fix.py: patched', path)
