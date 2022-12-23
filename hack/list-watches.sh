find /proc/${1}/fd \
    -lname anon_inode:inotify \
    -printf '%hinfo/%f\n' 2>/dev/null \
    \
    | xargs grep -c '^inotify'  \
    | sort -n -t: -k2 -r 
