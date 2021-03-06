"""
Contains the code for parsing of and the watcher for desktop files and a method to get information
about an application by the path of it
"""

from threading import Lock
import configparser

import threading
import glob
import os
import re

import pyinotify

DESKTOP_PATHS = tuple([
    os.path.join(d, 'applications')
    for d in os.getenv('XDG_DATA_DIRS', '/usr/share/').split(':')
])

class LinuxDesktopParser(threading.Thread):
    """
    Implements the parsing of and the watcher for desktop files
    """
    @classmethod
    def _parse_exec(cls, cmd):
        # remove stuff like %U
        cmd = re.sub(r'%[a-zA-Z]+', '', cmd)
        # remove 'env .... command'
        cmd = re.sub(r'^env\s+[^\s]+\s', '', cmd)
        # split && trim
        cmd = cmd.split(' ')[0].strip()
        # remove quotes
        cmd = re.sub(r'["\']+', '', cmd)
        # check if we need to resolve the path
        if len(cmd) > 0 and cmd[0] != '/':
            for path in os.environ["PATH"].split(os.pathsep):
                filename = os.path.join(path, cmd)
                if os.path.exists(filename):
                    cmd = filename
                    break
        return cmd

    def __init__(self):
        threading.Thread.__init__(self)
        self.lock = Lock()
        self.daemon = True
        self.running = False
        self.apps = {}
        # some things are just weird
        # (not really, i don't want to keep track of parent pids
        # just because of icons tho, this hack is way easier)
        self.fixes = {
            '/opt/google/chrome/chrome': '/opt/google/chrome/google-chrome',
            '/usr/lib/firefox/firefox': '/usr/lib/firefox/firefox.sh',
            '/usr/bin/pidgin.orig': '/usr/bin/pidgin'
        }

        for desktop_path in DESKTOP_PATHS:
            if not os.path.exists(desktop_path):
                continue
            for desktop_file in glob.glob(os.path.join(desktop_path, '*.desktop')):
                self._parse_desktop_file(desktop_file)

        self.start()


    def _parse_desktop_file(self, desktop_path):
        """
        Parse the desktop file at the given path and record it in self.apps
        """
        try:
            file = open(desktop_path, "r", encoding="utf-8", errors="replace")
            all_content = file.read()
            parser = configparser.ConfigParser(strict=False)  # Allow duplicate config entries
            parser.read_string(all_content)

            cmd = parser.get('Desktop Entry', 'exec', raw=True, fallback=None)
            if cmd is not None:
                cmd = self._parse_exec(cmd)
                icon = parser.get('Desktop Entry', 'Icon', raw=True, fallback=None)
                name = parser.get('Desktop Entry', 'Name', raw=True, fallback=None)
                with self.lock:
                    self.apps[cmd] = (name, icon, desktop_path)
                    # if the command is a symlink, add the real binary too
                    if os.path.islink(cmd):
                        link_to = os.path.realpath(cmd)
                        self.apps[link_to] = (name, icon, desktop_path)
        except:
            pass

    def get_info_by_path(self, path, default_icon):
        """
        Return the information about the application at the given path
        """
        def_name = os.path.basename(path)
        # apply fixes
        for orig, to in self.fixes.items():
            if path == orig:
                path = to
                break

        return self.apps.get(path, (def_name, default_icon, None))

    def run(self):
        """
        Start the watch manager
        """
        self.running = True
        watch_manager = pyinotify.WatchManager()
        notifier = pyinotify.Notifier(watch_manager)

        def inotify_callback(event):
            if event.mask == pyinotify.IN_CLOSE_WRITE:
                self._parse_desktop_file(event.pathname)

            elif event.mask == pyinotify.IN_DELETE:
                with self.lock:
                    for cmd, data in self.apps.items():
                        if data[2] == event.pathname:
                            del self.apps[cmd]
                            break

        for path in DESKTOP_PATHS:
            if os.path.exists(path):
                watch_manager.add_watch(
                    path,
                    pyinotify.IN_CLOSE_WRITE | pyinotify.IN_DELETE,
                    inotify_callback)
        notifier.loop()
