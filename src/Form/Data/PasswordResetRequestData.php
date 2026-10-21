<?php

declare(strict_types=1);

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetRequestData
{
    #[Assert\Email]
    #[Assert\NotBlank]
    public string $email;
}
