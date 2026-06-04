def fibonacci(n):
    """生成前 n 个斐波那契数列"""
    fib = []
    a, b = 0, 1
    for _ in range(n):
        fib.append(a)
        a, b = b, a + b
    return fib

# 计算前 10 个斐波那契数列
result = fibonacci(10)
print("前 10 个斐波那契数列：")
for i, num in enumerate(result, 1):
    print(f"第 {i:2d} 个：{num}")
