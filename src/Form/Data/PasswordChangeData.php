<?php
declare(strict_types=1);

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;
use Symfony\Component\Security\Core\Validator\Constraints as SecurityAssert;

class PasswordChangeData
{
    #[Assert\NotBlank]
    #[SecurityAssert\UserPassword(message: 'Wrong password')]
    public string $currentPassword;

    #[Assert\NotBlank]
    #[Assert\Length(min: 8, max: 4096)]
    #[Assert\NotCompromisedPassword(skipOnError: true)]
    public string $newPassword;
}
