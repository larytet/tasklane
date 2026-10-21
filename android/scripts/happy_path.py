#!/usr/bin/env python3
"""Happy path of the Android app, driven over adb on a running emulator (or device) with the debug APK installed.

  python3 android/scripts/happy_path.py [dark|light] [screenshot-prefix]

Walks sign-in, search, an issue and a comment, projects, a board, the inbox and sign-out, and stops at the first screen
that is not what it expects (saving a screenshot as <prefix>-FAIL.png). It needs a server reachable from the emulator at
http://10.0.2.2:18091 with: admin@example.com / adminpass1, a project SHOP (Webshop) with issues SHOP-1 "Checkout fails on
3DS challenge" (with a comment from another user whose text includes "I can reproduce this on staging"), SHOP-2 "Add Apple
Pay button", SHOP-3 "Product images load slowly", a board named "Webshop board", and one notification. Use a throwaway
server: the run adds a comment. Note it clears the app's data first.
"""
import subprocess, re, sys, time
import xml.etree.ElementTree as ET
ADB=["adb"]
def sh(*a): return subprocess.run(ADB+["shell",*a],capture_output=True,text=True).stdout
def dump():
    sh("uiautomator","dump","/sdcard/ui.xml")
    return ET.fromstring(subprocess.run(ADB+["exec-out","cat","/sdcard/ui.xml"],capture_output=True,text=True).stdout)
def find(text,exact=False):
    for n in dump().iter("node"):
        t=(n.get("text") or "")+"|"+(n.get("content-desc") or "")
        if (text in t.split("|")) if exact else (text.lower() in t.lower()):
            x1,y1,x2,y2=map(int,re.findall(r"\d+",n.get("bounds")));return (x1+x2)//2,(y1+y2)//2
def tap(text,exact=False,tries=10):
    for _ in range(tries):
        p=find(text,exact)
        if p: sh("input","tap",str(p[0]),str(p[1])); time.sleep(1.2); return p
        time.sleep(1)
    raise SystemExit(f"not found: {text}")
def typ(s): sh("input","text",s.replace(" ","%s")); time.sleep(.5)
def shot(path): open(path,"wb").write(subprocess.run(ADB+["exec-out","screencap","-p"],capture_output=True).stdout)
def has(text): return find(text) is not None

theme=sys.argv[1] if len(sys.argv)>1 else "dark"
out=sys.argv[2] if len(sys.argv)>2 else "/tmp/tasklane-android"
def expect(text,present=True,wait=8):
    for _ in range(wait):
        if has(text)==present: print("ok  ",("" if present else "no ")+text); return
        time.sleep(1)
    shot(f"{out}-FAIL.png"); raise SystemExit(f"FAIL: expected {'' if present else 'no '}{text!r}")
sh("cmd","uimode","night","yes" if theme=="dark" else "no")
sh("pm","clear","org.tasklane.android"); time.sleep(1)
sh("am","start","-n","org.tasklane.android/.MainActivity"); time.sleep(4)
expect("Sign in")
tap("Server"); typ("http://10.0.2.2:18091")
tap("Email"); typ("admin@example.com")
tap("Password"); typ("adminpass1"); sh("input","keyevent","4")
shot(f"{out}-1-sign-in.png")
tap("Sign in",exact=True)
expect("Checkout fails on 3DS challenge"); expect("Webshop",False,1)
shot(f"{out}-2-issues.png")
# search narrows the list
tap("Search issues"); typ("3DS"); sh("input","keyevent","66"); time.sleep(3)
expect("Checkout fails on 3DS challenge"); expect("Apple Pay",False)
shot(f"{out}-3-search.png")
# issue view, existing comments
tap("Checkout fails on 3DS"); expect("I can reproduce this on staging")
shot(f"{out}-4-issue.png")
# add a comment
tap("Add a comment"); typ("Fixed in staging, please retest")
tap("Post comment"); time.sleep(2); sh("input","keyevent","4"); time.sleep(1)  # hide the keyboard so swipes scroll the list
for _ in range(4): sh("input","swipe","540","1700","540","700","300"); time.sleep(.6)
expect("Fixed in staging")
shot(f"{out}-4b-commented.png")
tap("Back",exact=True); 
# projects -> project -> board
tap("Projects",exact=True); expect("Webshop")
shot(f"{out}-5-projects.png")
tap("Webshop"); expect("Webshop board"); tap("Webshop board"); expect("To Do"); expect("Product images load slowly")
shot(f"{out}-6-board.png")
tap("Back",exact=True); tap("Back",exact=True)
# inbox
tap("Inbox",exact=True); expect("Dana Levi commented")
shot(f"{out}-7-inbox.png")
# sign out
tap("Menu"); expect("Signed in as"); tap("Sign out"); expect("Sign in")
print("HAPPY PATH PASSED",theme)
